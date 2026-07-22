package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// hostRow mirrors shared_db::hosts::HostRow.
type hostRow struct {
	Slug             string  `json:"slug"`
	Addr             string  `json:"addr"`
	SSHUser          string  `json:"ssh_user"`
	SSHPort          int64   `json:"ssh_port"`
	SSHKeyPath       *string `json:"ssh_key_path"`
	Notes            string  `json:"notes"`
	CreatedAt        string  `json:"created_at"`
	RetiredAt        *string `json:"retired_at"`
	PasswordlessSudo bool    `json:"passwordless_sudo"`
}

// HostRegisterResult is the response shape for host_register.
type HostRegisterResult struct {
	OK               bool   `json:"ok"`
	HostID           string `json:"host_id"`
	PasswordlessSudo bool   `json:"passwordless_sudo"`
}

// HostRemoveResult is the response shape for host_remove.
type HostRemoveResult struct {
	OK      bool   `json:"ok"`
	HostID  string `json:"host_id"`
	Retired bool   `json:"retired"`
}

// hostRegisterParams / hostListParams / hostRemoveParams are the typed request
// bodies for the host CRUD actions — json.Unmarshal targets AND action-doc TYPE
// sources: adminActionRegistry reflects each so param types derive from the field
// kinds rather than being re-authored (chain finalize-action-docs-epic T4, bug
// 943; docs/ACTION_DOC_CONTRACT.md). Hoisted from the prior inline anonymous
// structs — same fields, json tags, and unmarshal, byte-for-byte unchanged. The
// required-ness (host_register: id/hostname/ssh_user; host_remove: id) is enforced
// by the handler guards below, so the descriptors author Required=true for those.
type hostRegisterParams struct {
	ID               string `json:"id"`
	Hostname         string `json:"hostname"`
	SSHUser          string `json:"ssh_user"`
	SSHPort          int64  `json:"ssh_port"`
	SSHKey           string `json:"ssh_key"`
	Description      string `json:"description"`
	PasswordlessSudo bool   `json:"passwordless_sudo"`
}

type hostListParams struct {
	IncludeRetired bool `json:"include_retired"`
}

type hostRemoveParams struct {
	ID string `json:"id"`
}

func (d Deps) hostRegister(ctx context.Context, params json.RawMessage) (HostRegisterResult, error) {
	var p hostRegisterParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return HostRegisterResult{}, err
		}
	}
	if p.ID == "" || p.Hostname == "" || p.SSHUser == "" {
		return HostRegisterResult{}, errors.New("params.id, params.hostname, and params.ssh_user are required")
	}
	if p.SSHPort == 0 {
		p.SSHPort = 22
	}
	// ssh_key_path is nullable in the schema; *string serialises as NULL
	// to database/sql when the pointer is nil.
	var sshKey *string
	if p.SSHKey != "" {
		sshKey = &p.SSHKey
	}
	passwordlessInt := int64(0)
	if p.PasswordlessSudo {
		passwordlessInt = 1
	}
	_, err := d.Pool.DB().ExecContext(ctx,
		`INSERT INTO hosts (slug, addr, ssh_user, ssh_port, ssh_key_path, notes, passwordless_sudo)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (slug) DO UPDATE SET
		    addr              = excluded.addr,
		    ssh_user          = excluded.ssh_user,
		    ssh_port          = excluded.ssh_port,
		    ssh_key_path      = excluded.ssh_key_path,
		    notes             = excluded.notes,
		    passwordless_sudo = excluded.passwordless_sudo`,
		p.ID, p.Hostname, p.SSHUser, p.SSHPort, sshKey, p.Description, passwordlessInt,
	)
	if err != nil {
		return HostRegisterResult{}, err
	}
	return HostRegisterResult{
		OK:               true,
		HostID:           p.ID,
		PasswordlessSudo: p.PasswordlessSudo,
	}, nil
}

func (d Deps) hostList(ctx context.Context, params json.RawMessage) ([]hostRow, error) {
	var p hostListParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	sqlStr := `SELECT slug, addr, ssh_user, ssh_port, ssh_key_path, notes, created_at,
	                   retired_at, passwordless_sudo FROM hosts`
	if p.IncludeRetired {
		sqlStr += ` ORDER BY slug`
	} else {
		sqlStr += ` WHERE retired_at IS NULL ORDER BY slug`
	}
	rows, err := d.Pool.DB().QueryContext(ctx, sqlStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hostRow{}
	for rows.Next() {
		var h hostRow
		var sudoInt int64
		if err := rows.Scan(&h.Slug, &h.Addr, &h.SSHUser, &h.SSHPort,
			&h.SSHKeyPath, &h.Notes, &h.CreatedAt, &h.RetiredAt, &sudoInt); err != nil {
			return nil, err
		}
		h.PasswordlessSudo = sudoInt != 0
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d Deps) hostRemove(ctx context.Context, params json.RawMessage) (HostRemoveResult, error) {
	var p hostRemoveParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return HostRemoveResult{}, err
		}
	}
	if p.ID == "" {
		return HostRemoveResult{}, errors.New("params.id is required")
	}
	// Match Rust find-then-retire so callers get a structured
	// host_not_found error instead of a silent 0-rows-affected UPDATE.
	var existing string
	err := d.Pool.DB().QueryRowContext(ctx,
		`SELECT slug FROM hosts WHERE slug = ?`, p.ID).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return HostRemoveResult{}, fmt.Errorf("host_not_found: %s", p.ID)
	}
	if err != nil {
		return HostRemoveResult{}, err
	}
	if _, err := d.Pool.DB().ExecContext(ctx,
		`UPDATE hosts SET retired_at = datetime('now')
		 WHERE slug = ? AND retired_at IS NULL`, p.ID); err != nil {
		return HostRemoveResult{}, err
	}
	return HostRemoveResult{OK: true, HostID: p.ID, Retired: true}, nil
}
