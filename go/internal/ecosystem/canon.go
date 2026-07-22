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

// canon.go extends the ecosystem surface with the artifact-identity / canonical-
// names map (follow-on to chain 435; suggestion
// extract-canonical-names-identity-map-into-deterministic-service). Where
// access_check answers "can I reach X", canon_resolve answers "what is X really
// called / where does it live, and is this name current or retired?" — resolving
// a stale alias (mcp-servers, ~/dev/mcp-servers, :3000) to its canonical form so
// stale-canon stops leaking. Direct-write, ships empty, learns as data.

// --- canon_resolve ----------------------------------------------------------

// CanonRecord is the deterministic canonical-identity answer, returned by both
// the canon_resolve action and the refresolve canon resolver (one source of
// truth).
type CanonRecord struct {
	Query       string `json:"query"`
	Resolved    bool   `json:"resolved"`
	Slug        string `json:"slug,omitempty"`
	Kind        string `json:"kind,omitempty"` // repo | path | project | db | port | service | other
	Canonical   string `json:"canonical,omitempty"`
	Status      string `json:"status,omitempty"` // current | retired
	Replacement string `json:"replacement,omitempty"`
	GiteaOwner  string `json:"gitea_owner,omitempty"`
	LocalPath   string `json:"local_path,omitempty"`
	Port        *int64 `json:"port,omitempty"`
	MatchedVia  string `json:"matched_via,omitempty"` // slug | canonical | local_path | alias
	SoftRef     string `json:"soft_ref,omitempty"`
	Answer      string `json:"answer"`
}

type canonResolveParams struct {
	Token string `json:"token"`
}

// canonResolve is the deterministic "what is the canonical form of X" query.
func (d Deps) canonResolve(ctx context.Context, params json.RawMessage) (CanonRecord, error) {
	var p canonResolveParams
	if len(params) == 0 {
		return CanonRecord{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return CanonRecord{}, err
	}
	if strings.TrimSpace(p.Token) == "" {
		return CanonRecord{}, errors.New("params.token is required")
	}
	return ResolveCanon(ctx, d.Pool.DB(), p.Token)
}

// ResolveCanon is the pure deterministic resolver. Exported so the refresolve
// canon resolver reuses the exact same logic canon_resolve runs — the
// parse_context orient-time answer and the explicit query can never diverge.
func ResolveCanon(ctx context.Context, h *sql.DB, token string) (CanonRecord, error) {
	norm := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), ":")))
	rec := CanonRecord{Query: token}

	// 1. Direct hit on an entry (slug / canonical value / local_path).
	slug, via, err := matchCanonEntry(ctx, h, norm)
	if err != nil {
		return rec, err
	}
	// 2. Else an alias.
	if slug == "" {
		if err := h.QueryRowContext(ctx,
			`SELECT entry_slug FROM canon_aliases WHERE lower(alias) = ? OR lower(alias) = ?`,
			norm, ":"+norm,
		).Scan(&slug); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				rec.Answer = fmt.Sprintf("Unknown: %q is not in the canonical-names map. Learn it with ecosystem.canon_learn.", token)
				return rec, nil
			}
			return rec, err
		}
		via = "alias"
	}
	return loadCanonEntry(ctx, h, slug, via)
}

func matchCanonEntry(ctx context.Context, h *sql.DB, norm string) (slug, via string, err error) {
	err = h.QueryRowContext(ctx, `SELECT slug FROM canon_entries WHERE lower(slug) = ?`, norm).Scan(&slug)
	if err == nil {
		return slug, "slug", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	err = h.QueryRowContext(ctx,
		`SELECT slug FROM canon_entries WHERE lower(canonical) = ? OR (local_path != '' AND lower(local_path) = ?)`,
		norm, norm).Scan(&slug)
	if err == nil {
		return slug, "canonical", nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return "", "", err
}

func loadCanonEntry(ctx context.Context, h *sql.DB, slug, via string) (CanonRecord, error) {
	rec := CanonRecord{Resolved: true, Slug: slug, MatchedVia: via}
	var port sql.NullInt64
	if err := h.QueryRowContext(ctx,
		`SELECT kind, canonical, status, replacement, gitea_owner, local_path, port, soft_ref
		   FROM canon_entries WHERE slug = ?`, slug,
	).Scan(&rec.Kind, &rec.Canonical, &rec.Status, &rec.Replacement, &rec.GiteaOwner, &rec.LocalPath, &port, &rec.SoftRef); err != nil {
		return rec, fmt.Errorf("read canon entry %q: %w", slug, err)
	}
	if port.Valid {
		rec.Port = &port.Int64
	}
	rec.Query = slug
	rec.Answer = composeCanonAnswer(rec)
	return rec, nil
}

func composeCanonAnswer(rec CanonRecord) string {
	loc := ""
	if rec.LocalPath != "" {
		loc = " at " + rec.LocalPath
	}
	if rec.Status == "retired" {
		repl := rec.Replacement
		if repl == "" {
			repl = "(no replacement recorded)"
		}
		return fmt.Sprintf("%q is RETIRED — the canonical %s is now %s.", rec.Canonical, rec.Kind, repl)
	}
	owner := ""
	if rec.GiteaOwner != "" {
		owner = fmt.Sprintf(" (gitea owner: %s)", rec.GiteaOwner)
	}
	return fmt.Sprintf("Canonical %s: %s%s%s — current.", rec.Kind, rec.Canonical, loc, owner)
}

// --- canon_learn ------------------------------------------------------------

type canonLearnParams struct {
	Slug        string   `json:"slug"`
	Kind        string   `json:"kind"`
	Canonical   string   `json:"canonical"`
	Status      string   `json:"status"` // current | retired (default current)
	Replacement string   `json:"replacement"`
	GiteaOwner  string   `json:"gitea_owner"`
	LocalPath   string   `json:"local_path"`
	Port        *int64   `json:"port"`
	Aliases     []string `json:"aliases"`
	Notes       string   `json:"notes"`
	SoftRef     string   `json:"soft_ref"`
}

var validCanonKinds = map[string]struct{}{
	"repo": {}, "path": {}, "project": {}, "db": {}, "port": {}, "service": {}, "other": {},
}

// canonLearn upserts a canonical entry + replaces its alias set (declarative).
func (d Deps) canonLearn(ctx context.Context, params json.RawMessage) (LearnResult, error) {
	var p canonLearnParams
	if len(params) == 0 {
		return LearnResult{}, errors.New("params required")
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return LearnResult{}, err
	}
	p.Slug = strings.TrimSpace(p.Slug)
	if p.Slug == "" || strings.TrimSpace(p.Canonical) == "" {
		return LearnResult{}, errors.New("params.slug and params.canonical are required")
	}
	if _, ok := validCanonKinds[p.Kind]; !ok {
		return LearnResult{}, fmt.Errorf("kind %q invalid (want repo|path|project|db|port|service|other)", p.Kind)
	}
	if p.Status == "" {
		p.Status = "current"
	}
	if p.Status != "current" && p.Status != "retired" {
		return LearnResult{}, fmt.Errorf("status %q invalid (want current|retired)", p.Status)
	}

	tx, err := d.Pool.DB().BeginTx(ctx, nil)
	if err != nil {
		return LearnResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO canon_entries (slug, kind, canonical, status, replacement, gitea_owner, local_path, port, notes, soft_ref, retired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CASE WHEN ?='retired' THEN datetime('now') ELSE NULL END)
		 ON CONFLICT (slug) DO UPDATE SET
		    kind=excluded.kind, canonical=excluded.canonical, status=excluded.status,
		    replacement=excluded.replacement, gitea_owner=excluded.gitea_owner,
		    local_path=excluded.local_path, port=excluded.port, notes=excluded.notes,
		    soft_ref=excluded.soft_ref,
		    retired_at=CASE WHEN excluded.status='retired' THEN datetime('now') ELSE NULL END`,
		p.Slug, p.Kind, p.Canonical, p.Status, p.Replacement, p.GiteaOwner, p.LocalPath, p.Port, p.Notes, p.SoftRef, p.Status,
	); err != nil {
		return LearnResult{}, fmt.Errorf("upsert canon entry: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM canon_aliases WHERE entry_slug = ?`, p.Slug); err != nil {
		return LearnResult{}, fmt.Errorf("clear canon aliases: %w", err)
	}
	for _, a := range p.Aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO canon_aliases (alias, entry_slug, dimension) VALUES (?, ?, 'other')
			 ON CONFLICT (alias) DO UPDATE SET entry_slug=excluded.entry_slug`,
			a, p.Slug,
		); err != nil {
			return LearnResult{}, fmt.Errorf("insert canon alias %q: %w", a, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return LearnResult{}, err
	}
	return LearnResult{OK: true, Kind: "canon_entry", Slug: p.Slug}, nil
}

// --- canon_list -------------------------------------------------------------

// CanonEntryRow is a compact row for canon_list.
type CanonEntryRow struct {
	Slug      string   `json:"slug"`
	Kind      string   `json:"kind"`
	Canonical string   `json:"canonical"`
	Status    string   `json:"status"`
	Aliases   []string `json:"aliases,omitempty"`
}

// CanonListResult enumerates the canonical-names map.
type CanonListResult struct {
	Entries []CanonEntryRow `json:"entries"`
	Count   int             `json:"count"`
}

// canonList enumerates the canonical-names map with each entry's aliases.
func (d Deps) canonList(ctx context.Context, _ json.RawMessage) (CanonListResult, error) {
	h := d.Pool.DB()
	res := CanonListResult{Entries: []CanonEntryRow{}}
	rows, err := h.QueryContext(ctx,
		`SELECT slug, kind, canonical, status FROM canon_entries ORDER BY status, kind, slug`)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var r CanonEntryRow
		if err := rows.Scan(&r.Slug, &r.Kind, &r.Canonical, &r.Status); err != nil {
			return res, err
		}
		res.Entries = append(res.Entries, r)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	for i := range res.Entries {
		al, err := aliasesFor(ctx, h, res.Entries[i].Slug)
		if err != nil {
			return res, err
		}
		res.Entries[i].Aliases = al
	}
	res.Count = len(res.Entries)
	return res, nil
}

func aliasesFor(ctx context.Context, h *sql.DB, slug string) ([]string, error) {
	rows, err := h.QueryContext(ctx, `SELECT alias FROM canon_aliases WHERE entry_slug = ? ORDER BY alias`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CanonTokens returns every recognizable canon token — entry slugs, canonical
// values, local paths, and all aliases — for the refresolve detector catalog.
// Deterministic (sorted, deduped). Mirrors AllTokens.
func CanonTokens(ctx context.Context, h *sql.DB) ([]string, error) {
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
	if err := collect(`SELECT slug FROM canon_entries`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT canonical FROM canon_entries`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT local_path FROM canon_entries WHERE local_path != ''`); err != nil {
		return nil, err
	}
	if err := collect(`SELECT alias FROM canon_aliases`); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}
