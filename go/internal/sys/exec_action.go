package sys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExecParams is the typed param struct for sys.exec. Command is required. The
// environment is intentionally NOT a caller param — the runner inherits the
// server environment; exposing env overrides to the model is unnecessary for the
// covered workflows and widens the surface.
type ExecParams struct {
	Command   string `json:"command"`
	Cwd       string `json:"cwd,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
	Sandbox   string `json:"sandbox,omitempty"` // none | bwrap | podman
}

// defaultExecAllowlist is the built-in set of command heads sys.exec permits.
// It deliberately EXCLUDES shells and eval-style binaries (sh/bash/zsh/env/eval/
// exec) — allowing them would let a model run anything through the gate. Extend
// per-deployment via TOOLKIT_EXEC_ALLOWLIST (comma-separated), never per-call.
var defaultExecAllowlist = []string{
	"git", "go", "make", "cargo", "npm", "node", "python3", "python",
	"ls", "cat", "head", "tail", "wc", "grep", "rg", "find", "echo", "pwd",
	"test", "true", "false", "sort", "uniq", "sed", "awk", "cut", "tr",
	"cd", "mkdir", "stat", "file", "du", "df",
	"podman", "docker", "systemctl", "curl",
}

// commandAllowlist gates which command heads sys.exec will run.
type commandAllowlist struct {
	allowed map[string]bool
}

// defaultAllowlist returns the built-in allowlist with no environment extension.
func defaultAllowlist() commandAllowlist {
	return loadAllowlist("")
}

// loadAllowlist builds the allowlist from the built-in default plus a
// comma-separated extension string (e.g. the TOOLKIT_EXEC_ALLOWLIST env var).
func loadAllowlist(extra string) commandAllowlist {
	set := make(map[string]bool, len(defaultExecAllowlist))
	for _, c := range defaultExecAllowlist {
		set[c] = true
	}
	for _, c := range strings.Split(extra, ",") {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	return commandAllowlist{allowed: set}
}

// permit reports nil when every command head in the (possibly compound) command
// is allowlisted and the command contains no command substitution; otherwise it
// returns an error explaining the rejection.
func (a commandAllowlist) permit(command string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("empty command")
	}
	// Command substitution can smuggle a disallowed command past head checks.
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		return errors.New("command substitution ($(...) or backticks) is not permitted in gated exec")
	}
	heads := commandHeads(command)
	if len(heads) == 0 {
		return errors.New("no command found")
	}
	for _, h := range heads {
		if !a.allowed[h] {
			return fmt.Errorf("command %q is not in the exec allowlist (extend via TOOLKIT_EXEC_ALLOWLIST)", h)
		}
	}
	return nil
}

// commandHeads extracts the head (executable basename) of every segment of a
// shell command, splitting on the operators ; | &. Leading VAR=value
// assignments are skipped; the head is the basename of the first real token.
func commandHeads(command string) []string {
	segments := strings.FieldsFunc(command, func(r rune) bool {
		return r == ';' || r == '|' || r == '&'
	})
	var heads []string
	for _, seg := range segments {
		for _, tok := range strings.Fields(seg) {
			if isEnvAssignment(tok) {
				continue // skip FOO=bar prefixes
			}
			heads = append(heads, filepath.Base(tok))
			break
		}
	}
	return heads
}

// isEnvAssignment reports whether tok looks like a leading VAR=value assignment.
func isEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for _, r := range tok[:eq] {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// execService holds the persistent runner + allowlist behind the sys.exec
// action. The runner's working directory persists across exec calls.
type execService struct {
	runner *Runner
	allow  commandAllowlist
}

// newExecService builds the exec service rooted at the process working
// directory, with the allowlist extended by TOOLKIT_EXEC_ALLOWLIST.
func newExecService() (*execService, error) {
	r, err := NewRunner("")
	if err != nil {
		return nil, err
	}
	return &execService{runner: r, allow: loadAllowlist(os.Getenv("TOOLKIT_EXEC_ALLOWLIST"))}, nil
}

// handle is the sys.exec dispatch handler. The rationale half of the gate is
// enforced upstream by dispatch-policy (requires_rationale); this enforces the
// allowlist half, then runs the command through the model-agnostic runner.
func (s *execService) handle(ctx context.Context, params json.RawMessage) (RunResult, error) {
	var p ExecParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RunResult{}, fmt.Errorf("sys.exec: invalid params: %w", err)
		}
	}
	if strings.TrimSpace(p.Command) == "" {
		return RunResult{}, errors.New("sys.exec requires command")
	}
	if err := s.allow.permit(p.Command); err != nil {
		return RunResult{}, fmt.Errorf("sys.exec: %w", err)
	}
	return s.runner.Run(ctx, p.Command, RunOptions{
		Cwd:       p.Cwd,
		TimeoutMS: p.TimeoutMS,
		Sandbox:   SandboxKind(p.Sandbox),
	})
}
