package ecosystem

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AccessStatus is the deterministic verdict of an access query.
type AccessStatus string

const (
	// StatusYes: target found and has >=1 enabled, usable access method.
	StatusYes AccessStatus = "yes"
	// StatusNo: target found but no usable access method recorded.
	StatusNo AccessStatus = "no"
	// StatusUnknown: target not learned. NEVER a hallucinated "no" — the
	// empty-tenant guardrail. Answer tells the agent to learn it.
	StatusUnknown AccessStatus = "unknown"
)

// AccessMethodView is one way to reach a target, for a query result.
type AccessMethodView struct {
	Method            string `json:"method"`
	Principal         string `json:"principal,omitempty"`
	Address           string `json:"address,omitempty"`
	CredentialPointer string `json:"credential_pointer,omitempty"`
	ScopeNote         string `json:"scope_note,omitempty"`
	Enabled           bool   `json:"enabled"`
	Source            string `json:"source"` // "host-inline" | "access-method"
}

// AccessSummary is the deterministic answer both the access_check action and the
// refresolve ecosystem resolver return — one source of truth for "do I have
// access to X".
type AccessSummary struct {
	Target        string             `json:"target"`
	Resolved      bool               `json:"resolved"`
	Kind          string             `json:"kind,omitempty"` // "host" | "service"
	Slug          string             `json:"slug,omitempty"`
	MatchedVia    string             `json:"matched_via,omitempty"`
	Status        AccessStatus       `json:"status"`
	AccessMethods []AccessMethodView `json:"access_methods,omitempty"`
	Addresses     []string           `json:"addresses,omitempty"`
	Endpoint      string             `json:"endpoint,omitempty"`       // service
	ServiceStatus string             `json:"service_status,omitempty"` // live|retired
	HostSlug      string             `json:"host_slug,omitempty"`      // a service's host
	SoftRefs      []string           `json:"soft_refs,omitempty"`
	Answer        string             `json:"answer"`
}

type accessCheckParams struct {
	Target string `json:"target"`
	Intent string `json:"intent"` // access | locate | describe (default access)
}

// accessCheck is the deterministic "do I have access to X" query.
func (d Deps) accessCheck(ctx context.Context, params json.RawMessage) (AccessSummary, error) {
	var p accessCheckParams
	if len(params) == 0 {
		return AccessSummary{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return AccessSummary{}, err
	}
	if strings.TrimSpace(p.Target) == "" {
		return AccessSummary{}, errors.New("params.target is required")
	}
	return ResolveAccess(ctx, d.Pool.DB(), p.Target)
}

// ResolveAccess is the pure deterministic resolver. Exported so the refresolve
// ecosystem resolver reuses the exact same logic the access_check action runs —
// the parse_context orient-time answer and the explicit query can never diverge.
func ResolveAccess(ctx context.Context, h *sql.DB, target string) (AccessSummary, error) {
	norm := strings.ToLower(strings.TrimSpace(target))
	sum := AccessSummary{Target: target, Status: StatusUnknown}

	// 1. Host by slug / address.
	if host, via, err := matchHost(ctx, h, norm); err != nil {
		return sum, err
	} else if host != "" {
		return resolveHost(ctx, h, host, via)
	}

	// 2. Service by slug.
	if svc, err := matchService(ctx, h, norm); err != nil {
		return sum, err
	} else if svc != "" {
		return resolveService(ctx, h, svc)
	}

	sum.Answer = fmt.Sprintf("Unknown: %q is not in the learned ecosystem. Learn it with ecosystem.host_learn / service_learn before relying on an access answer.", target)
	return sum, nil
}

// matchHost returns (host_slug, matched_via) for a non-retired host matched by
// exact slug, canonical addr, or an alternate address value.
func matchHost(ctx context.Context, h *sql.DB, norm string) (slug, via string, err error) {
	err = h.QueryRowContext(ctx,
		`SELECT slug FROM hosts WHERE lower(slug) = ? AND retired_at IS NULL`, norm).Scan(&slug)
	if err == nil {
		return slug, "slug", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	err = h.QueryRowContext(ctx,
		`SELECT slug FROM hosts WHERE lower(addr) = ? AND retired_at IS NULL`, norm).Scan(&slug)
	if err == nil {
		return slug, "address", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	err = h.QueryRowContext(ctx,
		`SELECT host_slug FROM ecosystem_host_addresses WHERE lower(value) = ?`, norm).Scan(&slug)
	if err == nil {
		return slug, "address", nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return "", "", err
}

func matchService(ctx context.Context, h *sql.DB, norm string) (string, error) {
	var slug string
	err := h.QueryRowContext(ctx,
		`SELECT slug FROM ecosystem_services WHERE lower(slug) = ?`, norm).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return slug, nil
}

func resolveHost(ctx context.Context, h *sql.DB, slug, via string) (AccessSummary, error) {
	sum := AccessSummary{Resolved: true, Kind: "host", Slug: slug, MatchedVia: via}

	var addr, sshUser string
	var sshKey sql.NullString
	if err := h.QueryRowContext(ctx,
		`SELECT addr, ssh_user, ssh_key_path FROM hosts WHERE slug = ?`, slug,
	).Scan(&addr, &sshUser, &sshKey); err != nil {
		return sum, fmt.Errorf("read host %q: %w", slug, err)
	}
	sum.Target = slug

	addrs, preferred, err := hostAddresses(ctx, h, slug, addr)
	if err != nil {
		return sum, err
	}
	sum.Addresses = addrs

	// The host's inline SSH fields ARE an access method.
	if strings.TrimSpace(sshUser) != "" {
		sum.AccessMethods = append(sum.AccessMethods, AccessMethodView{
			Method:            "ssh",
			Principal:         sshUser,
			Address:           preferred,
			CredentialPointer: sshKey.String,
			Enabled:           true,
			Source:            "host-inline",
		})
	}

	extra, softRefs, err := accessMethodsFor(ctx, h, "host", slug)
	if err != nil {
		return sum, err
	}
	sum.AccessMethods = append(sum.AccessMethods, extra...)
	sum.SoftRefs = softRefs

	sum.Status = statusFor(sum.AccessMethods)
	sum.Answer = composeHostAnswer(sum, preferred)
	return sum, nil
}

func resolveService(ctx context.Context, h *sql.DB, slug string) (AccessSummary, error) {
	sum := AccessSummary{Resolved: true, Kind: "service", Slug: slug, MatchedVia: "slug", Target: slug}

	var hostSlug, endpoint, status, softRef string
	if err := h.QueryRowContext(ctx,
		`SELECT host_slug, endpoint, status, soft_ref FROM ecosystem_services WHERE slug = ?`, slug,
	).Scan(&hostSlug, &endpoint, &status, &softRef); err != nil {
		return sum, fmt.Errorf("read service %q: %w", slug, err)
	}
	sum.HostSlug = hostSlug
	sum.Endpoint = endpoint
	sum.ServiceStatus = status
	if softRef != "" {
		sum.SoftRefs = append(sum.SoftRefs, softRef)
	}

	// Service-specific access methods, else fall back to the host's methods.
	methods, softRefs, err := accessMethodsFor(ctx, h, "service", slug)
	if err != nil {
		return sum, err
	}
	sum.SoftRefs = append(sum.SoftRefs, softRefs...)
	if len(methods) == 0 && hostSlug != "" {
		hostSum, err := resolveHost(ctx, h, hostSlug, "service-host")
		if err != nil {
			return sum, err
		}
		methods = hostSum.AccessMethods
		sum.SoftRefs = append(sum.SoftRefs, hostSum.SoftRefs...)
	}
	sum.AccessMethods = methods
	sum.SoftRefs = dedupe(sum.SoftRefs)

	sum.Status = statusFor(sum.AccessMethods)
	sum.Answer = composeServiceAnswer(sum)
	return sum, nil
}

func hostAddresses(ctx context.Context, h *sql.DB, slug, canonical string) (all []string, preferred string, err error) {
	seen := map[string]struct{}{}
	add := func(v string) {
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		all = append(all, v)
	}
	rows, err := h.QueryContext(ctx,
		`SELECT value, preferred FROM ecosystem_host_addresses WHERE host_slug = ? ORDER BY preferred DESC, value`, slug)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var pref int64
		if err := rows.Scan(&v, &pref); err != nil {
			return nil, "", err
		}
		add(v)
		if pref != 0 && preferred == "" {
			preferred = v
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	add(canonical)
	if preferred == "" {
		preferred = canonical
	}
	if preferred == "" && len(all) > 0 {
		preferred = all[0]
	}
	return all, preferred, nil
}

func accessMethodsFor(ctx context.Context, h *sql.DB, kind, slug string) ([]AccessMethodView, []string, error) {
	rows, err := h.QueryContext(ctx,
		`SELECT method, principal, credential_pointer, scope_note, enabled, soft_ref
		   FROM ecosystem_access_methods
		  WHERE target_kind = ? AND target_slug = ? AND retired_at IS NULL
		  ORDER BY enabled DESC, method`, kind, slug)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var methods []AccessMethodView
	var softRefs []string
	for rows.Next() {
		var m AccessMethodView
		var enabled int64
		var softRef string
		if err := rows.Scan(&m.Method, &m.Principal, &m.CredentialPointer, &m.ScopeNote, &enabled, &softRef); err != nil {
			return nil, nil, err
		}
		m.Enabled = enabled != 0
		m.Source = "access-method"
		methods = append(methods, m)
		if softRef != "" {
			softRefs = append(softRefs, softRef)
		}
	}
	return methods, softRefs, rows.Err()
}

func statusFor(methods []AccessMethodView) AccessStatus {
	for _, m := range methods {
		if m.Enabled && m.Method != "none" {
			return StatusYes
		}
	}
	return StatusNo
}

func composeHostAnswer(sum AccessSummary, preferred string) string {
	if sum.Status != StatusYes {
		return fmt.Sprintf("No recorded access to host %q (it is learned, but no usable access method is on file).", sum.Slug)
	}
	for _, m := range sum.AccessMethods {
		if m.Enabled && m.Method == "ssh" {
			key := ""
			if m.CredentialPointer != "" {
				key = fmt.Sprintf(" (key %s)", m.CredentialPointer)
			}
			return fmt.Sprintf("Yes — ssh %s@%s%s.", m.Principal, preferred, key)
		}
	}
	m := firstEnabled(sum.AccessMethods)
	return fmt.Sprintf("Yes — reach host %q via %s%s.", sum.Slug, m.Method, principalSuffix(m))
}

func composeServiceAnswer(sum AccessSummary) string {
	loc := sum.Slug
	if sum.Endpoint != "" {
		loc = sum.Endpoint
	}
	if sum.ServiceStatus == "retired" {
		return fmt.Sprintf("Service %q is RETIRED (was on host %q).", sum.Slug, sum.HostSlug)
	}
	if sum.Status != StatusYes {
		return fmt.Sprintf("Service %q is at %s on host %q, but no usable access method is on file.", sum.Slug, loc, sum.HostSlug)
	}
	m := firstEnabled(sum.AccessMethods)
	return fmt.Sprintf("Yes — service %q is at %s (host %q); reach it via %s%s.", sum.Slug, loc, sum.HostSlug, m.Method, principalSuffix(m))
}

func principalSuffix(m AccessMethodView) string {
	if m.Principal == "" {
		return ""
	}
	return " as " + m.Principal
}

func firstEnabled(methods []AccessMethodView) AccessMethodView {
	for _, m := range methods {
		if m.Enabled && m.Method != "none" {
			return m
		}
	}
	if len(methods) > 0 {
		return methods[0]
	}
	return AccessMethodView{}
}

func dedupe(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := map[string]struct{}{}
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// AllTokens returns the recognizable ecosystem tokens — host slugs, service
// slugs, and host addresses — for the refresolve detector catalog. Deterministic
// (sorted, deduped). Mirrors the chain/task/bug slug catalog loading.
func AllTokens(ctx context.Context, h *sql.DB) ([]string, error) {
	seen := map[string]struct{}{}
	collect := func(query string) error {
		rows, err := h.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return err
			}
			if v = strings.TrimSpace(v); v != "" {
				seen[v] = struct{}{}
			}
		}
		return rows.Err()
	}
	if err := collect(`SELECT slug FROM hosts WHERE retired_at IS NULL`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT slug FROM ecosystem_services`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT value FROM ecosystem_host_addresses`); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}
