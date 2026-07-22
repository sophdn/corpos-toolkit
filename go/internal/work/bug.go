package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"toolkit/internal/db"
	"toolkit/internal/dbutil"
	"toolkit/internal/events"
)

// Bug mirrors Rust work-lib::types::Bug. Full row from the bugs table.
type Bug struct {
	ID                 int64  `json:"id"`
	ProjectID          string `json:"project_id"`
	Slug               string `json:"slug"`
	Title              string `json:"title"`
	ProblemStatement   string `json:"problem_statement"`
	Surface            string `json:"surface"`
	Severity           string `json:"severity"`
	Source             string `json:"source"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Constraints        string `json:"constraints"`
	Status             string `json:"status"`
	// resolution_note retired from the projection in migration 065
	// (Phase 4 F2). It rides on BugResolved.payload.resolution_note —
	// readers wanting it follow the entity's event timeline.
	ResolutionKind       string `json:"resolution_kind,omitempty"`
	RoutedChainSlug      string `json:"routed_chain_slug"`
	RoutedTaskSlug       string `json:"routed_task_slug"`
	RoutedSuggestionSlug string `json:"routed_suggestion_slug"`
	ResolvedCommitSHA    string `json:"resolved_commit_sha,omitempty"`
	QwenTaskID           string `json:"qwen_task_id,omitempty"`
	Tags                 string `json:"tags"`
	FiledAt              string `json:"filed_at"`
	ResolvedAt           string `json:"resolved_at,omitempty"`
	UpdatedAt            string `json:"updated_at"`
}

// BugListTitle is the lightest projection bug_list returns when
// `titles_only=true`. Five fields are enough to scan a list and pick
// a slug for follow-up bug_read.
type BugListTitle struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
}

// BugListItem is the compact projection bug_list returns by default.
// Drops problem_statement / resolution_note / acceptance_criteria /
// constraints — agents that need them follow up via bug_read or
// list with verbose=true.
type BugListItem struct {
	ID                   int64  `json:"id"`
	ProjectID            string `json:"project_id"`
	Slug                 string `json:"slug"`
	Title                string `json:"title"`
	Surface              string `json:"surface"`
	Severity             string `json:"severity"`
	Source               string `json:"source"`
	Status               string `json:"status"`
	ResolutionKind       string `json:"resolution_kind,omitempty"`
	RoutedChainSlug      string `json:"routed_chain_slug"`
	RoutedTaskSlug       string `json:"routed_task_slug"`
	RoutedSuggestionSlug string `json:"routed_suggestion_slug"`
	ResolvedCommitSHA    string `json:"resolved_commit_sha,omitempty"`
	QwenTaskID           string `json:"qwen_task_id,omitempty"`
	Tags                 string `json:"tags"`
	FiledAt              string `json:"filed_at"`
	ResolvedAt           string `json:"resolved_at,omitempty"`
	UpdatedAt            string `json:"updated_at"`
}

// normalizeResolutionKind canonicalises verb-form aliases to past-participle.
// Mirrors the Rust normaliser plus bug 1330: external/externalized/upstreamed
// all map to 'upstream' — "real, reproducible, traceable to a dependency
// we don't author; not fixed locally for that reason". Distinct from
// wontfix (which keeps its prior 'out of scope / WAI' meaning).
func normalizeResolutionKind(k string) string {
	switch k {
	case "fix":
		return "fixed"
	case "route":
		return "routed"
	case "wont_fix":
		return "wontfix"
	case "duplicate":
		return "dup"
	case "external", "externalized", "upstreamed":
		return "upstream"
	}
	return k
}

// bugListParams captures every accepted bug_list filter / projection
// switch. Aliases (state, resolve_state) resolve to the canonical status
// via firstNonEmpty in HandleBugList.
type bugListParams struct {
	Status       string `json:"status"`
	State        string `json:"state"`
	ResolveState string `json:"resolve_state"`
	Severity     string `json:"severity"`
	Surface      string `json:"surface"`
	Since        string `json:"since"`
	Verbose      bool   `json:"verbose"`
	TitlesOnly   bool   `json:"titles_only"`
	All          bool   `json:"all"`
	Limit        int64  `json:"limit"`
	Offset       int64  `json:"offset"`
}

// HandleBugList implements work.bug_list. Scope resolution falls back to
// cross-project when args.Project is empty and no filters are supplied
// (mirrors roadmap_list / chain_status / searchTasksSummary). Bounded by
// the limit default (50) so the response stays small even on a full-table
// query. The legacy `all=true` field remains accepted but is now a no-op.
func HandleBugList(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (BugListResult, error) {
	p, err := decodeListParams[bugListParams](params, "bug_list",
		"status, severity, surface, since, verbose, titles_only, all, limit, offset (status aliases: state, resolve_state)")
	if err != nil {
		return BugListResult{}, err
	}
	status := firstNonEmpty(p.Status, p.State, p.ResolveState)

	limit, offset := normalizeLimitOffset(p.Limit, p.Offset, 50)
	surfacePattern := ""
	if p.Surface != "" {
		surfacePattern = "%" + p.Surface + "%"
	}
	wb := dbutil.NewWhereBuilder().
		Eq("b.project_id", project).
		Eq("b.status", status).
		Eq("b.severity", p.Severity).
		Like("b.surface", surfacePattern).
		GtEqString("b.filed_at", p.Since)
	whereClause := wb.Clause()
	args := wb.Args().Slice()

	limitClause := ""
	if limit >= 0 {
		if offset > 0 {
			limitClause = fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
		} else {
			limitClause = fmt.Sprintf("LIMIT %d", limit)
		}
	}

	// titles_only takes precedence over verbose — it's the most
	// restrictive projection, and asking for both is treated as
	// "give me the smallest one."
	if p.TitlesOnly {
		query := fmt.Sprintf(`SELECT b.id, b.slug, b.title, b.severity, b.status
			FROM proj_current_bugs b %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
		rows, err := pool.DB().QueryContext(ctx, query, args...)
		if err != nil {
			return BugListResult{}, err
		}
		defer rows.Close()
		titles, err := scanBugTitles(rows)
		if err != nil {
			return BugListResult{}, err
		}
		return BugListResult{TitlesOnly: true, TitlesItems: titles}, nil
	}

	if p.Verbose {
		query := fmt.Sprintf(`SELECT b.id, b.project_id, b.slug, b.title, b.problem_statement,
			b.surface, b.severity, b.source, b.acceptance_criteria, b.constraints,
			b.status, b.resolution_kind, b.routed_chain_slug, b.routed_task_slug,
			b.routed_suggestion_slug, b.resolved_commit_sha, b.qwen_task_id, b.tags,
			b.filed_at, b.resolved_at, b.updated_at
			FROM proj_current_bugs b %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
		rows, err := pool.DB().QueryContext(ctx, query, args...)
		if err != nil {
			return BugListResult{}, err
		}
		defer rows.Close()
		bugs, err := scanBugs(rows)
		if err != nil {
			return BugListResult{}, err
		}
		return BugListResult{Verbose: true, VerboseItems: bugs}, nil
	}

	query := fmt.Sprintf(`SELECT b.id, b.project_id, b.slug, b.title,
		b.surface, b.severity, b.source,
		b.status, b.resolution_kind, b.routed_chain_slug, b.routed_task_slug,
		b.routed_suggestion_slug, b.resolved_commit_sha, b.qwen_task_id, b.tags,
		b.filed_at, b.resolved_at, b.updated_at
		FROM proj_current_bugs b %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
	rows, err := pool.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return BugListResult{}, err
	}
	defer rows.Close()
	items, err := scanBugListItems(rows)
	if err != nil {
		return BugListResult{}, err
	}
	return BugListResult{DefaultItems: items}, nil
}

func scanBugs(rows *sql.Rows) ([]Bug, error) {
	// Non-nil zero-length slice so JSON marshals as `[]`, not `null`. Callers
	// rely on `[]` to distinguish "no matches" from "tool error".
	out := []Bug{}
	for rows.Next() {
		var b Bug
		var rkind, sha, qwen, resolvedAt sql.NullString
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Slug, &b.Title, &b.ProblemStatement,
			&b.Surface, &b.Severity, &b.Source, &b.AcceptanceCriteria, &b.Constraints,
			&b.Status, &rkind, &b.RoutedChainSlug, &b.RoutedTaskSlug,
			&b.RoutedSuggestionSlug, &sha, &qwen, &b.Tags, &b.FiledAt, &resolvedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.ResolutionKind = rkind.String
		b.ResolvedCommitSHA = sha.String
		b.QwenTaskID = qwen.String
		b.ResolvedAt = resolvedAt.String
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanBugTitles(rows *sql.Rows) ([]BugListTitle, error) {
	out := []BugListTitle{}
	for rows.Next() {
		var b BugListTitle
		if err := rows.Scan(&b.ID, &b.Slug, &b.Title, &b.Severity, &b.Status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func scanBugListItems(rows *sql.Rows) ([]BugListItem, error) {
	out := []BugListItem{}
	for rows.Next() {
		var b BugListItem
		var rkind, sha, qwen, resolvedAt sql.NullString
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Slug, &b.Title,
			&b.Surface, &b.Severity, &b.Source,
			&b.Status, &rkind, &b.RoutedChainSlug, &b.RoutedTaskSlug,
			&b.RoutedSuggestionSlug, &sha, &qwen, &b.Tags, &b.FiledAt, &resolvedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.ResolutionKind = rkind.String
		b.ResolvedCommitSHA = sha.String
		b.QwenTaskID = qwen.String
		b.ResolvedAt = resolvedAt.String
		out = append(out, b)
	}
	return out, rows.Err()
}

// bugReadParams accepts slug or id; id takes precedence when both arrive.
// Aliases (bug 1441): bug_id / bug_slug match the spelling sibling
// schemas + the action-docs scaffolding reach for.
type bugReadParams struct {
	Slug    string `json:"slug"`
	BugSlug string `json:"bug_slug"`
	ID      int64  `json:"id"`
	BugID   int64  `json:"bug_id"`
}

func (p bugReadParams) resolvedSlug() string { return firstNonEmpty(p.Slug, p.BugSlug) }
func (p bugReadParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.BugID
}

// HandleBugRead implements work.bug_read.
func HandleBugRead(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (BugReadResult, error) {
	var p bugReadParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BugReadResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if id := p.resolvedID(); id > 0 {
		return readBugByID(ctx, pool, id)
	}
	slug := p.resolvedSlug()
	if slug == "" {
		return BugReadResult{Err: &ErrorEnvelope{Error: IdentifierRequiredError("bug_read")}}, nil
	}
	return readBugBySlug(ctx, pool, slug)
}

func readBugBySlug(ctx context.Context, pool *db.Pool, slug string) (BugReadResult, error) {
	row := pool.DB().QueryRowContext(ctx, bugSelectByKey("slug"), slug)
	return scanOneBug(row, slug, "")
}

func readBugByID(ctx context.Context, pool *db.Pool, id int64) (BugReadResult, error) {
	row := pool.DB().QueryRowContext(ctx, bugSelectByKey("id"), id)
	return scanOneBug(row, "", fmt.Sprintf("%d", id))
}

func bugSelectByKey(key string) string {
	return `SELECT id, project_id, slug, title, problem_statement, surface, severity,
		source, acceptance_criteria, constraints,
		status, resolution_kind, routed_chain_slug, routed_task_slug,
		routed_suggestion_slug, resolved_commit_sha, qwen_task_id, tags,
		filed_at, resolved_at, updated_at
		FROM proj_current_bugs WHERE ` + key + ` = ?`
}

func scanOneBug(row *sql.Row, slug, idStr string) (BugReadResult, error) {
	var b Bug
	var rkind, sha, qwen, resolvedAt sql.NullString
	err := row.Scan(&b.ID, &b.ProjectID, &b.Slug, &b.Title, &b.ProblemStatement,
		&b.Surface, &b.Severity, &b.Source, &b.AcceptanceCriteria, &b.Constraints,
		&b.Status, &rkind, &b.RoutedChainSlug, &b.RoutedTaskSlug,
		&b.RoutedSuggestionSlug, &sha, &qwen, &b.Tags, &b.FiledAt, &resolvedAt, &b.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if slug != "" {
				return BugReadResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("bug '%s' not found", slug)}}, nil
			}
			return BugReadResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("bug id %s not found", idStr)}}, nil
		}
		return BugReadResult{}, err
	}
	b.ResolutionKind = rkind.String
	b.ResolvedCommitSHA = sha.String
	b.QwenTaskID = qwen.String
	b.ResolvedAt = resolvedAt.String
	return BugReadResult{Bug: &b}, nil
}

// bugResolveParams captures the resolve / reroute call shape. `kind` aliases
// `resolution_kind`; `sha` aliases `commit_sha`; `notes`,
// `resolution_summary`, and `summary` all alias `resolution_note`
// (bug 549 + bug 858 — agents reach for several paraphrases of the
// resolution-note field; all of them silently dropped pre-alias). `id`
// is a bug 1329 alias for `slug` — id-by-default flows (bug_list →
// bug_resolve) skip a lookup hop. Slug takes precedence when both are
// supplied.
type bugResolveParams struct {
	Slug                 string `json:"slug"`
	BugSlug              string `json:"bug_slug"` // bug 1441 alias
	ID                   int64  `json:"id"`
	BugID                int64  `json:"bug_id"` // bug 1441 alias
	ResolutionKind       string `json:"resolution_kind"`
	Kind                 string `json:"kind"`
	ResolutionNote       string `json:"resolution_note"`
	Notes                string `json:"notes"`              // bug 549 alias
	ResolutionSummary    string `json:"resolution_summary"` // bug 858 alias
	Summary              string `json:"summary"`            // bug 858 alias
	CommitSHA            string `json:"commit_sha"`
	SHA                  string `json:"sha"`
	RoutedChainSlug      string `json:"routed_chain_slug"`
	RoutedTaskSlug       string `json:"routed_task_slug"`
	RoutedSuggestionSlug string `json:"routed_suggestion_slug"`
}

func (p bugResolveParams) resolvedSlug() string { return firstNonEmpty(p.Slug, p.BugSlug) }
func (p bugResolveParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.BugID
}

// HandleBugResolve mirrors work-lib::bugs::resolve_bug. Normalises
// resolution_kind verb-form aliases verbatim; enforces the BUG_TRANSITIONS
// machine including the routed → routed reroute (preserves resolved_at)
// and routed → fixed (gated on non-empty commit_sha; also preserves
// resolved_at). All other status→status transitions are rejected.
func HandleBugResolve(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (BugResolveResult, error) {
	var result BugResolveResult
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		r, _, err := HandleBugResolveInTx(ctx, tx, pool, project, params)
		result = r
		return err
	})
	if err != nil && result.Error == "" {
		return BugResolveResult{}, err
	}
	return result, nil
}

// HandleBugResolveInTx is the tx-aware variant of HandleBugResolve.
// work.HandleBatch dispatches to this when bug_resolve appears as a
// sub-op inside a batch — the outer tx flows through so true rollback
// works when a later op fails. Returns (result, eventID, err) where
// eventID is the cascade BugResolved event's id (empty on failure).
//
// Reads (lookupBugSlugByID, fetchBugStatusAndProject) still use
// pool.DB(), so cross-op read-after-write inside a batch sees pre-batch
// state per the documented batch limitation. The smoke tests do not
// exercise cross-op read dependencies; if a real-world use case needs
// read-through-tx, the reads can be migrated to take *sql.Tx in a
// follow-up.
func HandleBugResolveInTx(ctx context.Context, tx *sql.Tx, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (BugResolveResult, string, error) {
	var p bugResolveParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BugResolveResult{}, "", fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" {
		p.Slug = p.BugSlug
	}
	if id := p.resolvedID(); p.Slug == "" && id > 0 {
		slug, err := lookupBugSlugByID(ctx, pool, id)
		if err != nil {
			return BugResolveResult{Error: err.Error()}, "", nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return BugResolveResult{Error: IdentifierRequiredError("bug_resolve")}, "", nil
	}
	rawKind := firstNonEmpty(p.ResolutionKind, p.Kind)
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	// Bug 1381: 'fix landed in commit X' is the dominant resolve shape by
	// a wide margin. When the caller supplies a SHA and omits the kind,
	// default to 'fixed' — sha+wontfix/dup/routed is incoherent, so
	// disambiguation only matters when no SHA is present.
	if rawKind == "" && sha != "" {
		rawKind = "fixed"
	}
	if rawKind == "" {
		return BugResolveResult{
			Error: "bug_resolve requires `resolution_kind` (or `kind` as alias) — one of: fixed, wontfix, upstream, dup, routed (verb-form aliases also accepted: fix, wont_fix, external/externalized/upstreamed, duplicate, route). Pass a commit_sha to default kind=fixed without naming it explicitly.",
		}, "", nil
	}
	kind := normalizeResolutionKind(rawKind)
	if kind != "fixed" && kind != "wontfix" && kind != "upstream" && kind != "dup" && kind != "routed" {
		return BugResolveResult{
			Error: fmt.Sprintf("invalid resolution_kind '%s', must be one of: fixed, wontfix, upstream, dup, routed", rawKind),
		}, "", nil
	}
	if sha != "" && !isValidCommitSHAOrSentinel(sha) {
		return BugResolveResult{
			Error:  shaValidationError(sha, true),
			Action: "bug_resolve",
		}, "", nil
	}

	current, projectID, err := fetchBugStatusAndProject(ctx, pool, p.Slug)
	if err != nil {
		return BugResolveResult{Error: err.Error()}, "", nil
	}

	if err := checkBugTransition(p.Slug, current, kind, sha); err != nil {
		return BugResolveResult{Error: err.Error()}, "", nil
	}

	isReroute := current == "routed" && kind == "routed"

	emitPayload := events.BugResolvedPayload{
		Kind:                 kind,
		CommitSHA:            strPtr(sha),
		RoutedChainSlug:      strPtr(p.RoutedChainSlug),
		RoutedTaskSlug:       strPtr(p.RoutedTaskSlug),
		RoutedSuggestionSlug: strPtr(p.RoutedSuggestionSlug),
		ResolutionNote:       strPtr(firstNonEmpty(p.ResolutionNote, p.Notes, p.ResolutionSummary, p.Summary)),
	}

	eventID, err := events.Emit(ctx, tx, events.EmitArgs{
		Entity:  events.NewEntityRef("bug", p.Slug, projectID),
		Payload: emitPayload,
	})
	if err != nil {
		return BugResolveResult{}, "", err
	}
	newStatus := kind
	if isReroute {
		newStatus = "routed"
	}
	return BugResolveResult{
		OK:     true,
		Slug:   p.Slug,
		Status: newStatus,
	}, eventID, nil
}

// bugTransitions mirrors transitions::BUG_TRANSITIONS exactly, with the
// `open → upstream` arm added in bug 1330 — the resolution_kind enum's
// new sibling needs an explicit transition or checkBugTransition rejects
// it as not-allowed.
var bugTransitions = []struct {
	From string
	To   string
	Gate string
}{
	{"open", "fixed", ""},
	{"open", "wontfix", ""},
	{"open", "upstream", ""},
	{"open", "dup", ""},
	{"open", "routed", ""},
	{"routed", "routed", ""},
	{"routed", "fixed", "commit_sha"},
}

func checkBugTransition(slug, from, to, commitSHA string) error {
	for _, t := range bugTransitions {
		if t.From != from || t.To != to {
			continue
		}
		if t.Gate == "" {
			return nil
		}
		if t.Gate == "commit_sha" {
			if commitSHA != "" {
				return nil
			}
			return fmt.Errorf("bug '%s' in status '%s': transition '%s' → '%s' requires non-empty 'commit_sha'", slug, from, from, to)
		}
	}
	if from != "open" {
		return fmt.Errorf("bug '%s' is in status '%s'; cannot resolve a non-open bug", slug, from)
	}
	return fmt.Errorf("bug '%s' in status '%s': transition '%s' → '%s' not allowed", slug, from, from, to)
}

func fetchBugStatus(ctx context.Context, pool *db.Pool, slug string) (string, error) {
	status, _, err := fetchBugStatusAndProject(ctx, pool, slug)
	return status, err
}

// fetchBugStatusAndProject returns both status and project_id in one query.
// Callers that need project_id (event emit constructs an EntityRef with it)
// use this to avoid a second round-trip.
func fetchBugStatusAndProject(ctx context.Context, pool *db.Pool, slug string) (string, string, error) {
	var status, projectID string
	err := pool.DB().QueryRowContext(ctx, `SELECT status, project_id FROM proj_current_bugs WHERE slug = ?`, slug).Scan(&status, &projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("bug '%s' not found", slug)
		}
		return "", "", err
	}
	return status, projectID, nil
}

// fetchBugResolutionSnapshot reads the current resolution-shape fields off
// a bug row. Used by HandleBugReopen to populate the
// BugReopenedPayload.PreviousResolution so the events ledger preserves
// the reversed resolution without forcing a walk back through events.
// Returns the project_id alongside so the caller has everything it needs
// to construct the EntityRef.
func fetchBugResolutionSnapshot(ctx context.Context, pool *db.Pool, slug string) (events.BugResolvedPayload, string, error) {
	var projectID, kind, routedChain, routedTask, sha sql.NullString
	var statusCol string
	err := pool.DB().QueryRowContext(ctx,
		`SELECT project_id, status, resolution_kind,
		        routed_chain_slug, routed_task_slug, resolved_commit_sha
		 FROM proj_current_bugs WHERE slug = ?`, slug,
	).Scan(&projectID, &statusCol, &kind, &routedChain, &routedTask, &sha)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return events.BugResolvedPayload{}, "", fmt.Errorf("bug '%s' not found", slug)
		}
		return events.BugResolvedPayload{}, "", err
	}
	// resolution_note retired from the projection in migration 065
	// (Phase 4 F2). BugReopened.previous_resolution.resolution_note
	// drops to nil; readers who want the original note follow the
	// BugResolved event payload directly via the events ledger.
	payload := events.BugResolvedPayload{Kind: kind.String}
	if payload.Kind == "" {
		payload.Kind = statusCol
	}
	if sha.Valid && sha.String != "" {
		s := sha.String
		payload.CommitSHA = &s
	}
	if routedChain.Valid && routedChain.String != "" {
		s := routedChain.String
		payload.RoutedChainSlug = &s
	}
	if routedTask.Valid && routedTask.String != "" {
		s := routedTask.String
		payload.RoutedTaskSlug = &s
	}
	return payload, projectID.String, nil
}

// strPtr returns a pointer to s when non-empty, nil otherwise. Used to
// build optional-field payloads from the bug handler's string-typed
// params without scattering `if s != "" { p := s; field = &p }`.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// lookupBugSlugByID resolves a bug id to its slug. Lets bug_resolve and
// bug_stamp_sha accept the same {id} alias bug_read already supports
// (bug 1329); the rest of each handler still operates on the slug so the
// write SQL stays identical regardless of how the caller named the bug.
func lookupBugSlugByID(ctx context.Context, pool *db.Pool, id int64) (string, error) {
	var slug string
	err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_current_bugs WHERE id = ?`, id).Scan(&slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("bug id %d not found", id)
		}
		return "", err
	}
	return slug, nil
}

// bugReopenParams + HandleBugReopen flip a resolved bug back to open,
// clearing the resolution audit fields. Mirrors work-lib::bugs::reopen_bug.
type bugReopenParams struct {
	Slug    string `json:"slug"`
	BugSlug string `json:"bug_slug"` // bug 1441 alias
	ID      int64  `json:"id"`       // bug 1441: also accept id, lookup → slug
	BugID   int64  `json:"bug_id"`
}

func HandleBugReopen(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (BugResolveResult, error) {
	var p bugReopenParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return BugResolveResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" {
		p.Slug = p.BugSlug
	}
	if id := firstNonZeroInt64(p.ID, p.BugID); p.Slug == "" && id > 0 {
		slug, err := lookupBugSlugByID(ctx, pool, id)
		if err != nil {
			return BugResolveResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return BugResolveResult{Error: IdentifierRequiredError("bug_reopen")}, nil
	}
	previousResolution, projectID, err := fetchBugResolutionSnapshot(ctx, pool, p.Slug)
	if err != nil {
		return BugResolveResult{Error: err.Error()}, nil
	}
	if previousResolution.Kind == "open" {
		return BugResolveResult{Error: fmt.Sprintf("bug '%s' is already open", p.Slug)}, nil
	}
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-bugs: CRUD UPDATE dropped; fold reopens proj_current_bugs.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("bug", p.Slug, projectID),
			Payload: events.BugReopenedPayload{PreviousResolution: previousResolution},
		})
		return err
	})
	if err != nil {
		return BugResolveResult{}, err
	}
	return BugResolveResult{OK: true, Slug: p.Slug, Status: "open"}, nil
}

// bugStampParams accepts slug + commit_sha (sha alias). `id` mirrors the
// bug 1329 alias on bug_resolve so the bug_list → bug_stamp_sha flow can
// stay id-keyed end-to-end.
type bugStampParams struct {
	Slug      string `json:"slug"`
	BugSlug   string `json:"bug_slug"` // bug 1441 alias
	ID        int64  `json:"id"`
	BugID     int64  `json:"bug_id"` // bug 1441 alias
	CommitSHA string `json:"commit_sha"`
	SHA       string `json:"sha"`
}

// HandleBugStampSHA stamps resolved_commit_sha on an already-resolved bug.
// Refuses to stamp open bugs to keep the column meaningful.
func HandleBugStampSHA(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (ShaStampResult, error) {
	var p bugStampParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ShaStampResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	if p.Slug == "" {
		p.Slug = p.BugSlug
	}
	if id := firstNonZeroInt64(p.ID, p.BugID); p.Slug == "" && id > 0 {
		slug, err := lookupBugSlugByID(ctx, pool, id)
		if err != nil {
			return ShaStampResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return ShaStampResult{Error: IdentifierRequiredError("bug_stamp_sha")}, nil
	}
	if sha == "" {
		return ShaStampResult{Error: "commit_sha must not be empty"}, nil
	}
	if !isValidCommitSHAOrSentinel(sha) {
		return ShaStampResult{
			Error:  shaValidationError(sha, false),
			Action: "bug_stamp_sha",
		}, nil
	}
	current, projectID, err := fetchBugStatusAndProject(ctx, pool, p.Slug)
	if err != nil {
		return ShaStampResult{Error: err.Error()}, nil
	}
	if current == "open" {
		return ShaStampResult{
			Error: fmt.Sprintf("bug '%s' is still open; resolve it before stamping a SHA. Hint: bug_resolve accepts commit_sha directly — pass {resolution_kind: \"fixed\", commit_sha: \"<sha>\"} to resolve and stamp in one call.", p.Slug),
		}, nil
	}
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-bugs: CRUD UPDATE dropped; fold stamps the SHA on proj_current_bugs.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("bug", p.Slug, projectID),
			Payload: events.BugStampedPayload{CommitSHA: sha},
		})
		return err
	})
	if err != nil {
		return ShaStampResult{}, err
	}
	return ShaStampResult{OK: true, Slug: p.Slug, ResolvedCommitSHA: sha}, nil
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

var bugListDoc = ActionDoc{
	Purpose: "List bugs. Compact projection by default. Requires a project OR at least one filter; pass all=true to confirm a full-table dump.",
	Params: []DocParam{
		{Name: "status", Required: false, Description: "open / fixed / wontfix / routed / dup. Alias: state."},
		{Name: "state", Required: false, Description: "Alias of status.", AliasOf: "status"},
		{Name: "severity", Required: false, Description: "low / medium / high."},
		{Name: "surface", Required: false, Description: "Comma-separated surface tags; match-any."},
		{Name: "since", Required: false, Description: "ISO timestamp; surfaces bugs updated since."},
		{Name: "verbose", Required: false, Description: "Return full bodies including problem_statement."},
		{Name: "all", Required: false, Description: "Confirm a full-table dump when no project / filter passed."},
	},
	Example: `{"status":"open"}`,
	Errors: []ActionError{
		{Condition: "no project AND no filter", Message: "bug_list requires a project OR at least one filter (status, severity, surface, since); pass all=true to confirm a full-table dump."},
	},
}

var bugReadDoc = ActionDoc{
	Purpose: "Return a single bug's full content. Identify by id or slug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Bug id (preferred)."},
		{Name: "slug", Required: false, Description: "Bug slug. Either id or slug identifies the bug (bug 1329 parity)."},
	},
	Example: `{"id":1333}`,
	Notes:   "Numeric id is accepted in place of the slug; the dispatcher resolves either to the same row.",
}

var bugResolveDoc = ActionDoc{
	Purpose: "Resolve a bug (kind=fixed / wontfix / dup / routed). When commit_sha is supplied and kind is omitted, kind defaults to 'fixed' — the dominant 'fix landed in commit X' shape. For routed, supply routed_chain_slug + routed_task_slug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Bug id (preferred)."},
		{Name: "slug", Required: false, Description: "Bug slug. Either id or slug identifies the bug (bug 1329 parity)."},
		{Name: "resolution_kind", Required: false, Description: "fixed / wontfix / dup / routed. Alias: kind. Defaults to 'fixed' when commit_sha is supplied; required otherwise."},
		{Name: "kind", Required: false, Description: "Alias of resolution_kind.", AliasOf: "resolution_kind"},
		{Name: "resolution_note", Required: false, Description: "Free-form note. For kind=fixed: what changed. For wontfix: reasoning. For dup: target bug slug + note. Aliases: notes, resolution_summary, summary."},
		{Name: "notes", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "resolution_summary", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "summary", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "commit_sha", Required: false, Description: "SHA the fix landed in. Required for kind=fixed unless the artifact lives outside any git repo (then 'unversioned'). When set, defaults kind=fixed."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
		{Name: "routed_chain_slug", Required: false, Description: "When kind=routed, the chain absorbing this bug's work."},
		{Name: "routed_task_slug", Required: false, Description: "When kind=routed, the specific task in routed_chain_slug."},
		{Name: "routed_suggestion_slug", Required: false, Description: "When the bug's resolution surfaces a structural improvement worth proposing, the suggestion slug. Symmetric counterpart to suggestion.routed_bug_slug."},
	},
	Example: `{"slug":"my-bug","commit_sha":"abc1234","resolution_note":"Fixed by adding X"}`,
	ValueAliases: []ActionValueAlias{
		{Param: "resolution_kind", From: "fix", To: "fixed"},
		{Param: "resolution_kind", From: "route", To: "routed"},
		{Param: "resolution_kind", From: "wont_fix", To: "wontfix"},
		{Param: "resolution_kind", From: "duplicate", To: "dup"},
		{Param: "resolution_kind", From: "external", To: "upstream", Notes: "Also accepts externalized and upstreamed for the same canonical 'upstream' resolution."},
	},
	Errors: []ActionError{
		{Condition: "missing resolution_kind", Message: "Returns an explicit error naming all five accepted values."},
	},
	Notes:                "'upstream' (added in bug 1330) means \"real, reproducible, traceable to a dependency we don't author; not fixed locally for that reason\" — distinct from wontfix's \"out of scope / working as intended\". Verb-form aliases are normalized to the past-participle enum: fix→fixed, route→routed, wont_fix→wontfix, duplicate→dup, external/externalized/upstreamed→upstream.",
	EnvelopeRequirements: rationaleEnv(),
}

var bugReopenDoc = ActionDoc{
	Purpose: "Re-open a previously-resolved bug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Bug id (preferred)."},
		{Name: "slug", Required: false, Description: "Bug slug. Either id or slug identifies the bug (bug 1329 parity)."},
	},
	Example:              `{"slug":"my-bug"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var bugStampShaDoc = ActionDoc{
	Purpose: "Stamp a commit SHA on an already-resolved bug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Bug id (preferred)."},
		{Name: "slug", Required: false, Description: "Bug slug. Either id or slug identifies the bug (bug 1329 parity)."},
		{Name: "commit_sha", Required: true, Description: "SHA to stamp. Alias: sha. Accepts 'unversioned'."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
	},
	Example:              `{"slug":"my-bug","sha":"abc1234"}`,
	EnvelopeRequirements: rationaleEnv(),
}
