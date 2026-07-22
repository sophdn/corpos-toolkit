package ecosystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// --- host_learn -------------------------------------------------------------

// hostAddressInput is one alternate reachable address for a host.
type hostAddressInput struct {
	Kind      string `json:"kind"`  // tailnet | lan | magicdns | hostname | other
	Value     string `json:"value"` // '203.0.113.10', 'example-host.tailnet.ts.net'
	Preferred bool   `json:"preferred"`
	Notes     string `json:"notes"`
}

type hostLearnParams struct {
	Slug             string             `json:"slug"`
	Addr             string             `json:"addr"`
	SSHUser          string             `json:"ssh_user"`
	SSHPort          int64              `json:"ssh_port"`
	SSHKeyPath       string             `json:"ssh_key_path"` // POINTER, e.g. ~/.ssh/id_ed25519
	PasswordlessSudo bool               `json:"passwordless_sudo"`
	Notes            string             `json:"notes"`
	Addresses        []hostAddressInput `json:"addresses"`
}

// LearnResult is the shared response shape for the *_learn actions.
type LearnResult struct {
	OK   bool   `json:"ok"`
	Kind string `json:"kind"` // "host" | "service" | "access_method"
	Slug string `json:"slug"`
}

var validAddrKinds = map[string]struct{}{
	"tailnet": {}, "lan": {}, "magicdns": {}, "hostname": {}, "other": {},
}

// hostLearn upserts a host into the REUSED shared-infra `hosts` table (migration
// 003) and replaces its alternate-address set. The inline SSH fields
// (ssh_user/ssh_key_path) ARE the host's SSH access method — the query reads them
// directly, so a simple host_learn already answers "do I have ssh access".
func (d Deps) hostLearn(ctx context.Context, params json.RawMessage) (LearnResult, error) {
	var p hostLearnParams
	if len(params) == 0 {
		return LearnResult{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return LearnResult{}, err
	}
	p.Slug = strings.TrimSpace(p.Slug)
	if p.Slug == "" || strings.TrimSpace(p.Addr) == "" {
		return LearnResult{}, errors.New("params.slug and params.addr are required")
	}
	if err := rejectInlineSecret("ssh_key_path", p.SSHKeyPath); err != nil {
		return LearnResult{}, err
	}
	if p.SSHPort == 0 {
		p.SSHPort = 22
	}
	for _, a := range p.Addresses {
		if _, ok := validAddrKinds[a.Kind]; !ok {
			return LearnResult{}, fmt.Errorf("address kind %q invalid (want tailnet|lan|magicdns|hostname|other)", a.Kind)
		}
		if strings.TrimSpace(a.Value) == "" {
			return LearnResult{}, errors.New("each address needs a non-empty value")
		}
	}

	var sshKey *string
	if p.SSHKeyPath != "" {
		sshKey = &p.SSHKeyPath
	}
	sudo := int64(0)
	if p.PasswordlessSudo {
		sudo = 1
	}

	tx, err := d.Pool.DB().BeginTx(ctx, nil)
	if err != nil {
		return LearnResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO hosts (slug, addr, ssh_user, ssh_port, ssh_key_path, notes, passwordless_sudo)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (slug) DO UPDATE SET
		    addr=excluded.addr, ssh_user=excluded.ssh_user, ssh_port=excluded.ssh_port,
		    ssh_key_path=excluded.ssh_key_path, notes=excluded.notes,
		    passwordless_sudo=excluded.passwordless_sudo, retired_at=NULL`,
		p.Slug, p.Addr, p.SSHUser, p.SSHPort, sshKey, p.Notes, sudo,
	); err != nil {
		return LearnResult{}, fmt.Errorf("upsert host: %w", err)
	}

	// Replace the alternate-address set for this host (learn is declarative).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ecosystem_host_addresses WHERE host_slug = ?`, p.Slug); err != nil {
		return LearnResult{}, fmt.Errorf("clear host addresses: %w", err)
	}
	for _, a := range p.Addresses {
		pref := int64(0)
		if a.Preferred {
			pref = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ecosystem_host_addresses (host_slug, kind, value, preferred, notes)
			 VALUES (?, ?, ?, ?, ?)`,
			p.Slug, a.Kind, a.Value, pref, a.Notes,
		); err != nil {
			return LearnResult{}, fmt.Errorf("insert host address %q: %w", a.Value, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return LearnResult{}, err
	}
	return LearnResult{OK: true, Kind: "host", Slug: p.Slug}, nil
}

// --- service_learn ----------------------------------------------------------

type serviceLearnParams struct {
	Slug     string `json:"slug"`
	HostSlug string `json:"host_slug"`
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Port     *int64 `json:"port"`
	Status   string `json:"status"` // live | retired (default live)
	SoftRef  string `json:"soft_ref"`
	Notes    string `json:"notes"`
}

// serviceLearn upserts a service running on an already-learned host.
func (d Deps) serviceLearn(ctx context.Context, params json.RawMessage) (LearnResult, error) {
	var p serviceLearnParams
	if len(params) == 0 {
		return LearnResult{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return LearnResult{}, err
	}
	p.Slug = strings.TrimSpace(p.Slug)
	p.HostSlug = strings.TrimSpace(p.HostSlug)
	if p.Slug == "" || p.HostSlug == "" {
		return LearnResult{}, errors.New("params.slug and params.host_slug are required")
	}
	if p.Status == "" {
		p.Status = "live"
	}
	if p.Status != "live" && p.Status != "retired" {
		return LearnResult{}, fmt.Errorf("status %q invalid (want live|retired)", p.Status)
	}

	exists, err := hostExists(ctx, d.Pool.DB(), p.HostSlug)
	if err != nil {
		return LearnResult{}, err
	}
	if !exists {
		return LearnResult{}, fmt.Errorf("host_not_found: %s (learn the host first)", p.HostSlug)
	}

	// status drives retirement; retired_at is set via the CASE expressions below.
	if _, err := d.Pool.DB().ExecContext(ctx,
		`INSERT INTO ecosystem_services (slug, host_slug, kind, endpoint, port, status, soft_ref, notes, retired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CASE WHEN ?='retired' THEN datetime('now') ELSE NULL END)
		 ON CONFLICT (slug) DO UPDATE SET
		    host_slug=excluded.host_slug, kind=excluded.kind, endpoint=excluded.endpoint,
		    port=excluded.port, status=excluded.status, soft_ref=excluded.soft_ref,
		    notes=excluded.notes,
		    retired_at=CASE WHEN excluded.status='retired' THEN datetime('now') ELSE NULL END`,
		p.Slug, p.HostSlug, p.Kind, p.Endpoint, p.Port, p.Status, p.SoftRef, p.Notes, p.Status,
	); err != nil {
		return LearnResult{}, fmt.Errorf("upsert service: %w", err)
	}
	return LearnResult{OK: true, Kind: "service", Slug: p.Slug}, nil
}

// --- access_learn -----------------------------------------------------------

type accessLearnParams struct {
	Slug              string `json:"slug"`
	TargetKind        string `json:"target_kind"` // host | service
	TargetSlug        string `json:"target_slug"`
	Method            string `json:"method"` // ssh | https-api | https-basic | token | none
	Principal         string `json:"principal"`
	CredentialPointer string `json:"credential_pointer"` // POINTER only
	ScopeNote         string `json:"scope_note"`
	Enabled           *bool  `json:"enabled"` // default true
	SoftRef           string `json:"soft_ref"`
	Notes             string `json:"notes"`
}

var validAccessMethods = map[string]struct{}{
	"ssh": {}, "https-api": {}, "https-basic": {}, "token": {}, "none": {},
}

// accessLearn upserts an access method attached to a host or service.
func (d Deps) accessLearn(ctx context.Context, params json.RawMessage) (LearnResult, error) {
	var p accessLearnParams
	if len(params) == 0 {
		return LearnResult{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return LearnResult{}, err
	}
	p.Slug = strings.TrimSpace(p.Slug)
	p.TargetSlug = strings.TrimSpace(p.TargetSlug)
	if p.Slug == "" || p.TargetSlug == "" {
		return LearnResult{}, errors.New("params.slug and params.target_slug are required")
	}
	if p.TargetKind != "host" && p.TargetKind != "service" {
		return LearnResult{}, fmt.Errorf("target_kind %q invalid (want host|service)", p.TargetKind)
	}
	if _, ok := validAccessMethods[p.Method]; !ok {
		return LearnResult{}, fmt.Errorf("method %q invalid (want ssh|https-api|https-basic|token|none)", p.Method)
	}
	if err := rejectInlineSecret("credential_pointer", p.CredentialPointer); err != nil {
		return LearnResult{}, err
	}

	// Referential validation the polymorphic target can't express as an FK.
	ok, err := targetExists(ctx, d.Pool.DB(), p.TargetKind, p.TargetSlug)
	if err != nil {
		return LearnResult{}, err
	}
	if !ok {
		return LearnResult{}, fmt.Errorf("%s_not_found: %s (learn the target first)", p.TargetKind, p.TargetSlug)
	}

	enabled := int64(1)
	if p.Enabled != nil && !*p.Enabled {
		enabled = 0
	}
	if _, err := d.Pool.DB().ExecContext(ctx,
		`INSERT INTO ecosystem_access_methods
		    (slug, target_kind, target_slug, method, principal, credential_pointer, scope_note, enabled, soft_ref, notes, retired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		 ON CONFLICT (slug) DO UPDATE SET
		    target_kind=excluded.target_kind, target_slug=excluded.target_slug, method=excluded.method,
		    principal=excluded.principal, credential_pointer=excluded.credential_pointer,
		    scope_note=excluded.scope_note, enabled=excluded.enabled, soft_ref=excluded.soft_ref,
		    notes=excluded.notes, retired_at=NULL`,
		p.Slug, p.TargetKind, p.TargetSlug, p.Method, p.Principal, p.CredentialPointer,
		p.ScopeNote, enabled, p.SoftRef, p.Notes,
	); err != nil {
		return LearnResult{}, fmt.Errorf("upsert access method: %w", err)
	}
	return LearnResult{OK: true, Kind: "access_method", Slug: p.Slug}, nil
}

// --- shared helpers ---------------------------------------------------------

func hostExists(ctx context.Context, h *sql.DB, slug string) (bool, error) {
	var got string
	err := h.QueryRowContext(ctx, `SELECT slug FROM hosts WHERE slug = ?`, slug).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func targetExists(ctx context.Context, h *sql.DB, kind, slug string) (bool, error) {
	if kind == "host" {
		return hostExists(ctx, h, slug)
	}
	var got string
	err := h.QueryRowContext(ctx, `SELECT slug FROM ecosystem_services WHERE slug = ?`, slug).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// rejectInlineSecret enforces the credential-pointer invariant: the value must be
// a POINTER to where a secret lives (a path or env name), not the secret itself.
// A path/env-name is fine; a long unbroken alnum/base64 run with no path markers
// reads as an inline secret and is rejected. Empty is always allowed.
func rejectInlineSecret(field, value string) error {
	if looksLikeInlineSecret(value) {
		return fmt.Errorf(
			"%s looks like an inline secret; store a POINTER instead (a path or env name like ~/.ssh/id_ed25519 or GITEA_TOKEN), never the secret value",
			field)
	}
	return nil
}

func looksLikeInlineSecret(s string) bool {
	for _, tok := range strings.Fields(s) {
		// A path or shell-var reference is a legitimate pointer, never a secret.
		if strings.ContainsAny(tok, "/~.$") {
			continue
		}
		// A long unbroken token drawn only from secret/base64/hex alphabets,
		// with no path markers, is almost certainly an inline secret.
		if len([]rune(tok)) >= 24 && isSecretAlphabet(tok) {
			return true
		}
	}
	return false
}

func isSecretAlphabet(tok string) bool {
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '+' || r == '/' || r == '=' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
