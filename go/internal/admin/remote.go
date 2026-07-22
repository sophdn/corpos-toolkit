package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// remoteExecResponseCap caps stdout/stderr in the response payload.
// The full output (within the system_ssh stdout/err pipe buffer cap)
// is still persisted in the remote_ops audit row.
const remoteExecResponseCap = 50 * 1024

// RemoteExecResult is the response shape for remote_exec.
//
// remote_exec returns three response shapes (success, transport not
// implemented, host not registered, connection failed) as a single
// struct with omitempty fields — mirroring the Rust handler which
// produced different JSON object shapes depending on outcome.
//
// In success cases, ExitCode is a *int64 so that a nil pointer encodes
// as JSON null when the SSH connection failed before a process started.
type RemoteExecResult struct {
	// Success / connection_failed fields
	HostID     string `json:"host_id,omitempty"`
	Cmd        string `json:"cmd,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	ExitCode   *int64 `json:"exit_code,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	RemoteOpID int64  `json:"remote_op_id,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`

	// transport_not_implemented + host_not_registered + connection_failed envelopes
	Error            string   `json:"error,omitempty"`
	TransportBackend string   `json:"transport_backend,omitempty"`
	Supported        []string `json:"supported,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

// remoteExec ports crates/toolkit-server/src/dispatch/admin.rs's
// remote_exec. system_ssh is the only supported transport in the Go
// port; the russh transport stays Rust-only until concrete demand drives
// the port. system_ssh shells out to /usr/bin/ssh with -p, -i, and the
// resolved user/host from the hosts row.
// remoteExecParams is the typed remote_exec request body — the json.Unmarshal
// target AND the action-doc TYPE source: adminActionRegistry reflects it
// (reflect.TypeOf(remoteExecParams{})) so each param's type derives from the field
// kind rather than being re-authored (chain finalize-action-docs-epic T4, bug 943;
// docs/ACTION_DOC_CONTRACT.md). Hoisted from the prior inline anonymous struct —
// same fields, json tags, and unmarshal, byte-for-byte unchanged. host + cmd
// required-ness is enforced by the handler guard below, so the descriptor authors
// Required=true for them.
type remoteExecParams struct {
	TransportBackend string `json:"transport_backend"`
	Host             string `json:"host"`
	Cmd              string `json:"cmd"`
	ProjectID        string `json:"project_id"`
}

func (d Deps) remoteExec(ctx context.Context, params json.RawMessage) (RemoteExecResult, error) {
	var p remoteExecParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RemoteExecResult{}, err
		}
	}
	if p.TransportBackend == "" {
		p.TransportBackend = "system_ssh"
	}
	if p.TransportBackend != "system_ssh" {
		return RemoteExecResult{
			Error:            "transport_not_implemented",
			TransportBackend: p.TransportBackend,
			Supported:        []string{"system_ssh"},
			Detail:           "Go admin server only supports system_ssh; russh requires the archived Rust crate transport-lib.",
		}, nil
	}
	if p.Host == "" || p.Cmd == "" {
		return RemoteExecResult{}, errors.New("params.host and params.cmd are required")
	}
	if p.ProjectID == "" {
		// Mirrors Rust default — project housing the admin meta-tool.
		p.ProjectID = "mcp-servers"
	}

	host, err := d.loadHost(ctx, p.Host)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RemoteExecResult{Error: "host_not_registered", HostID: p.Host}, nil
		}
		return RemoteExecResult{}, err
	}

	start := time.Now()
	stdoutBuf, stderrBuf, exitCode, runErr := runSystemSSH(ctx, host, p.Cmd)
	duration := time.Since(start).Milliseconds()

	stdoutCapped, stdoutTruncated := capForResponse(stdoutBuf.String())
	stderrCapped, stderrTruncated := capForResponse(stderrBuf.String())

	// exit_code is nullable in remote_ops; nil pointer here serialises
	// as NULL via database/sql.
	var exitInt64 *int64
	if exitCode != nil {
		v := int64(*exitCode)
		exitInt64 = &v
	}
	res, dbErr := d.Pool.DB().ExecContext(ctx,
		`INSERT INTO remote_ops
		   (project_id, host_slug, kind, command, stdout, stderr, exit_code, duration_ms, finished_at)
		 VALUES (?, ?, 'command', ?, ?, ?, ?, ?, datetime('now'))`,
		p.ProjectID, p.Host, p.Cmd,
		stdoutBuf.String(), stderrBuf.String(), exitInt64, duration,
	)
	if dbErr != nil {
		// Audit insert failure is a hard error — without it the call is
		// invisible. Surface to caller rather than swallowing.
		return RemoteExecResult{}, fmt.Errorf("audit insert: %w", dbErr)
	}
	auditID, _ := res.LastInsertId()

	if runErr != nil {
		return RemoteExecResult{
			Error:  "connection_failed",
			HostID: p.Host,
			Detail: runErr.Error(),
		}, nil
	}

	return RemoteExecResult{
		HostID:     p.Host,
		Cmd:        p.Cmd,
		Stdout:     stdoutCapped,
		Stderr:     stderrCapped,
		ExitCode:   exitInt64,
		DurationMs: duration,
		RemoteOpID: auditID,
		Truncated:  stdoutTruncated || stderrTruncated,
	}, nil
}

func (d Deps) loadHost(ctx context.Context, slug string) (*hostLookup, error) {
	var h hostLookup
	err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT slug, addr, ssh_user, ssh_port, ssh_key_path
		 FROM hosts WHERE slug = ?`, slug).
		Scan(&h.Slug, &h.Addr, &h.SSHUser, &h.SSHPort, &h.SSHKeyPath)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

type hostLookup struct {
	Slug       string
	Addr       string
	SSHUser    string
	SSHPort    int64
	SSHKeyPath sql.NullString
}

func runSystemSSH(ctx context.Context, host *hostLookup, cmd string) (stdout, stderr bytes.Buffer, exitCode *int, err error) {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-p", strconv.FormatInt(host.SSHPort, 10),
	}
	if host.SSHKeyPath.Valid && host.SSHKeyPath.String != "" {
		args = append(args, "-i", host.SSHKeyPath.String)
	}
	args = append(args, host.SSHUser+"@"+host.Addr, cmd)

	c := exec.CommandContext(ctx, "ssh", args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if runErr := c.Run(); runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			return stdout, stderr, &code, nil
		}
		// Connection / fork failure — exit_code stays nil so the
		// audit row reflects the "command never ran" state and the
		// caller gets connection_failed.
		return stdout, stderr, nil, runErr
	}
	code := 0
	return stdout, stderr, &code, nil
}

func capForResponse(s string) (string, bool) {
	if len(s) <= remoteExecResponseCap {
		return s, false
	}
	return s[:remoteExecResponseCap], true
}
