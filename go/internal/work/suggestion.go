package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/events"
)

// Suggestion is the full row from the suggestions table. Mirrors Bug's
// shape but with native vocabulary throughout (priority not severity;
// routed_bug_slug as the symmetric pair to bugs.routed_suggestion_slug).
// Per chain `agent-suggestion-box` design_decisions, no shared helper
// is extracted across bug / suggestion handlers — duplication is cheap;
// premature abstraction would push per-vocab branching into the helper.
type Suggestion struct {
	ID                 int64  `json:"id"`
	ProjectID          string `json:"project_id"`
	Slug               string `json:"slug"`
	Title              string `json:"title"`
	ProblemStatement   string `json:"problem_statement"`
	Surface            string `json:"surface"`
	Priority           string `json:"priority"`
	Source             string `json:"source"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	Constraints        string `json:"constraints"`
	Status             string `json:"status"`
	// resolution_note retired from the projection in migration 065
	// (Phase 4 F2). It rides on SuggestionResolved.payload.resolution_note —
	// readers wanting it follow the entity's event timeline.
	ResolutionKind    string `json:"resolution_kind,omitempty"`
	RoutedChainSlug   string `json:"routed_chain_slug"`
	RoutedTaskSlug    string `json:"routed_task_slug"`
	RoutedBugSlug     string `json:"routed_bug_slug"`
	ResolvedCommitSHA string `json:"resolved_commit_sha,omitempty"`
	Tags              string `json:"tags"`
	FiledAt           string `json:"filed_at"`
	ResolvedAt        string `json:"resolved_at,omitempty"`
	UpdatedAt         string `json:"updated_at"`
}

// SuggestionListTitle is the lightest projection suggestion_list returns
// when `titles_only=true`. Five fields are enough to scan a list and pick
// a slug for follow-up suggestion_read.
type SuggestionListTitle struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// SuggestionListItem is the compact projection suggestion_list returns by
// default. Drops problem_statement / resolution_note / acceptance_criteria
// / constraints — agents that need them follow up via suggestion_read or
// list with verbose=true.
type SuggestionListItem struct {
	ID                int64  `json:"id"`
	ProjectID         string `json:"project_id"`
	Slug              string `json:"slug"`
	Title             string `json:"title"`
	Surface           string `json:"surface"`
	Priority          string `json:"priority"`
	Source            string `json:"source"`
	Status            string `json:"status"`
	ResolutionKind    string `json:"resolution_kind,omitempty"`
	RoutedChainSlug   string `json:"routed_chain_slug"`
	RoutedTaskSlug    string `json:"routed_task_slug"`
	RoutedBugSlug     string `json:"routed_bug_slug"`
	ResolvedCommitSHA string `json:"resolved_commit_sha,omitempty"`
	Tags              string `json:"tags"`
	FiledAt           string `json:"filed_at"`
	ResolvedAt        string `json:"resolved_at,omitempty"`
	UpdatedAt         string `json:"updated_at"`
}

// normalizeSuggestionResolutionKind canonicalises verb-form aliases to
// past-participle. Mirrors the bug-side normaliser shape but with the
// suggestion vocab (adopted/deferred/rejected). The aliases reflect the
// most common verb forms agents reach for: adopt → adopted,
// defer → deferred, reject → rejected.
func normalizeSuggestionResolutionKind(k string) string {
	switch k {
	case "adopt":
		return "adopted"
	case "defer":
		return "deferred"
	case "reject":
		return "rejected"
	}
	return k
}

// suggestionListParams captures every accepted suggestion_list filter +
// projection switch. Mirrors bugListParams' alias triple (status / state /
// resolve_state) for shape parity.
type suggestionListParams struct {
	Status       string `json:"status"`
	State        string `json:"state"`
	ResolveState string `json:"resolve_state"`
	Priority     string `json:"priority"`
	Surface      string `json:"surface"`
	Since        string `json:"since"`
	Verbose      bool   `json:"verbose"`
	TitlesOnly   bool   `json:"titles_only"`
	All          bool   `json:"all"`
	Limit        int64  `json:"limit"`
	Offset       int64  `json:"offset"`
}

// HandleSuggestionList implements work.suggestion_list. Scope resolution
// falls back to cross-project when args.Project is empty and no filters
// are supplied — mirrors HandleBugList's bug-1310 cross-project shape.
// Bounded by the limit default (50). `all=true` is accepted as a legacy
// no-op for shape parity.
func HandleSuggestionList(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (SuggestionListResult, error) {
	p, err := decodeListParams[suggestionListParams](params, "suggestion_list",
		"status, priority, surface, since, verbose, titles_only, all, limit, offset (status aliases: state, resolve_state)")
	if err != nil {
		return SuggestionListResult{}, err
	}
	status := firstNonEmpty(p.Status, p.State, p.ResolveState)

	limit, offset := normalizeLimitOffset(p.Limit, p.Offset, 50)
	args := db.NewArgs()
	var conds []string
	if project != "" {
		conds = append(conds, "s.project_id = ?")
		args.AddString(project)
	}
	if status != "" {
		conds = append(conds, "s.status = ?")
		args.AddString(status)
	}
	if p.Priority != "" {
		conds = append(conds, "s.priority = ?")
		args.AddString(p.Priority)
	}
	if p.Surface != "" {
		conds = append(conds, "s.surface LIKE ?")
		args.AddString("%" + p.Surface + "%")
	}
	if p.Since != "" {
		conds = append(conds, "s.filed_at >= ?")
		args.AddString(p.Since)
	}
	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}

	limitClause := ""
	if limit >= 0 {
		if offset > 0 {
			limitClause = fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
		} else {
			limitClause = fmt.Sprintf("LIMIT %d", limit)
		}
	}

	// titles_only takes precedence over verbose — most restrictive
	// projection wins, same as bug_list.
	if p.TitlesOnly {
		query := fmt.Sprintf(`SELECT s.id, s.slug, s.title, s.priority, s.status
			FROM proj_current_suggestions s %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
		rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
		if err != nil {
			return SuggestionListResult{}, err
		}
		defer rows.Close()
		titles, err := scanSuggestionTitles(rows)
		if err != nil {
			return SuggestionListResult{}, err
		}
		return SuggestionListResult{TitlesOnly: true, TitlesItems: titles}, nil
	}

	if p.Verbose {
		query := fmt.Sprintf(`SELECT s.id, s.project_id, s.slug, s.title, s.problem_statement,
			s.surface, s.priority, s.source, s.acceptance_criteria, s.constraints,
			s.status, s.resolution_kind, s.routed_chain_slug, s.routed_task_slug,
			s.routed_bug_slug, s.resolved_commit_sha, s.tags, s.filed_at, s.resolved_at, s.updated_at
			FROM proj_current_suggestions s %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
		rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
		if err != nil {
			return SuggestionListResult{}, err
		}
		defer rows.Close()
		suggestions, err := scanSuggestions(rows)
		if err != nil {
			return SuggestionListResult{}, err
		}
		return SuggestionListResult{Verbose: true, VerboseItems: suggestions}, nil
	}

	query := fmt.Sprintf(`SELECT s.id, s.project_id, s.slug, s.title,
		s.surface, s.priority, s.source,
		s.status, s.resolution_kind, s.routed_chain_slug, s.routed_task_slug,
		s.routed_bug_slug, s.resolved_commit_sha, s.tags, s.filed_at, s.resolved_at, s.updated_at
		FROM proj_current_suggestions s %s ORDER BY filed_at DESC %s`, whereClause, limitClause)
	rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
	if err != nil {
		return SuggestionListResult{}, err
	}
	defer rows.Close()
	items, err := scanSuggestionListItems(rows)
	if err != nil {
		return SuggestionListResult{}, err
	}
	return SuggestionListResult{DefaultItems: items}, nil
}

func scanSuggestions(rows *sql.Rows) ([]Suggestion, error) {
	out := []Suggestion{}
	for rows.Next() {
		var s Suggestion
		var rkind, sha, resolvedAt sql.NullString
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Slug, &s.Title, &s.ProblemStatement,
			&s.Surface, &s.Priority, &s.Source, &s.AcceptanceCriteria, &s.Constraints,
			&s.Status, &rkind, &s.RoutedChainSlug, &s.RoutedTaskSlug,
			&s.RoutedBugSlug, &sha, &s.Tags, &s.FiledAt, &resolvedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.ResolutionKind = rkind.String
		s.ResolvedCommitSHA = sha.String
		s.ResolvedAt = resolvedAt.String
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSuggestionTitles(rows *sql.Rows) ([]SuggestionListTitle, error) {
	out := []SuggestionListTitle{}
	for rows.Next() {
		var s SuggestionListTitle
		if err := rows.Scan(&s.ID, &s.Slug, &s.Title, &s.Priority, &s.Status); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSuggestionListItems(rows *sql.Rows) ([]SuggestionListItem, error) {
	out := []SuggestionListItem{}
	for rows.Next() {
		var s SuggestionListItem
		var rkind, sha, resolvedAt sql.NullString
		if err := rows.Scan(&s.ID, &s.ProjectID, &s.Slug, &s.Title,
			&s.Surface, &s.Priority, &s.Source,
			&s.Status, &rkind, &s.RoutedChainSlug, &s.RoutedTaskSlug,
			&s.RoutedBugSlug, &sha, &s.Tags, &s.FiledAt, &resolvedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.ResolutionKind = rkind.String
		s.ResolvedCommitSHA = sha.String
		s.ResolvedAt = resolvedAt.String
		out = append(out, s)
	}
	return out, rows.Err()
}

// suggestionReadParams accepts slug or id; id takes precedence when both
// arrive. Mirrors bug 1329's id-alias parity on bug_read; suggestion_id /
// suggestion_slug match the convention sibling schemas + action-docs reach
// for.
type suggestionReadParams struct {
	Slug           string `json:"slug"`
	SuggestionSlug string `json:"suggestion_slug"`
	ID             int64  `json:"id"`
	SuggestionID   int64  `json:"suggestion_id"`
}

func (p suggestionReadParams) resolvedSlug() string {
	return firstNonEmpty(p.Slug, p.SuggestionSlug)
}
func (p suggestionReadParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.SuggestionID
}

// HandleSuggestionRead implements work.suggestion_read.
func HandleSuggestionRead(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (SuggestionReadResult, error) {
	var p suggestionReadParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return SuggestionReadResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if id := p.resolvedID(); id > 0 {
		return readSuggestionByID(ctx, pool, id)
	}
	slug := p.resolvedSlug()
	if slug == "" {
		return SuggestionReadResult{Err: &ErrorEnvelope{Error: IdentifierRequiredError("suggestion_read")}}, nil
	}
	return readSuggestionBySlug(ctx, pool, slug)
}

func readSuggestionBySlug(ctx context.Context, pool *db.Pool, slug string) (SuggestionReadResult, error) {
	row := pool.DB().QueryRowContext(ctx, suggestionSelectByKey("slug"), slug)
	return scanOneSuggestion(row, slug, "")
}

func readSuggestionByID(ctx context.Context, pool *db.Pool, id int64) (SuggestionReadResult, error) {
	row := pool.DB().QueryRowContext(ctx, suggestionSelectByKey("id"), id)
	return scanOneSuggestion(row, "", fmt.Sprintf("%d", id))
}

func suggestionSelectByKey(key string) string {
	return `SELECT id, project_id, slug, title, problem_statement, surface, priority,
		source, acceptance_criteria, constraints,
		status, resolution_kind, routed_chain_slug, routed_task_slug,
		routed_bug_slug, resolved_commit_sha, tags, filed_at, resolved_at, updated_at
		FROM proj_current_suggestions WHERE ` + key + ` = ?`
}

func scanOneSuggestion(row *sql.Row, slug, idStr string) (SuggestionReadResult, error) {
	var s Suggestion
	var rkind, sha, resolvedAt sql.NullString
	err := row.Scan(&s.ID, &s.ProjectID, &s.Slug, &s.Title, &s.ProblemStatement,
		&s.Surface, &s.Priority, &s.Source, &s.AcceptanceCriteria, &s.Constraints,
		&s.Status, &rkind, &s.RoutedChainSlug, &s.RoutedTaskSlug,
		&s.RoutedBugSlug, &sha, &s.Tags, &s.FiledAt, &resolvedAt, &s.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if slug != "" {
				return SuggestionReadResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("suggestion '%s' not found", slug)}}, nil
			}
			return SuggestionReadResult{Err: &ErrorEnvelope{Error: fmt.Sprintf("suggestion id %s not found", idStr)}}, nil
		}
		return SuggestionReadResult{}, err
	}
	s.ResolutionKind = rkind.String
	s.ResolvedCommitSHA = sha.String
	s.ResolvedAt = resolvedAt.String
	return SuggestionReadResult{Suggestion: &s}, nil
}

// suggestionResolveParams captures the resolve / re-adopt call shape.
// `kind` aliases `resolution_kind`; `sha` aliases `commit_sha`;
// `notes`, `resolution_summary`, and `summary` all alias
// `resolution_note` (bug 549 + bug 858 siblings — symmetric with the
// bug_resolve aliases for the same friction class). `id` is a bug
// 1329 alias for `slug` — id-by-default flows skip a lookup hop.
type suggestionResolveParams struct {
	Slug              string `json:"slug"`
	SuggestionSlug    string `json:"suggestion_slug"`
	ID                int64  `json:"id"`
	SuggestionID      int64  `json:"suggestion_id"`
	ResolutionKind    string `json:"resolution_kind"`
	Kind              string `json:"kind"`
	ResolutionNote    string `json:"resolution_note"`
	Notes             string `json:"notes"`              // bug 549 alias
	ResolutionSummary string `json:"resolution_summary"` // bug 858 alias
	Summary           string `json:"summary"`            // bug 858 alias
	CommitSHA         string `json:"commit_sha"`
	SHA               string `json:"sha"`
	RoutedChainSlug   string `json:"routed_chain_slug"`
	RoutedTaskSlug    string `json:"routed_task_slug"`
	RoutedBugSlug     string `json:"routed_bug_slug"`
}

func (p suggestionResolveParams) resolvedSlug() string {
	return firstNonEmpty(p.Slug, p.SuggestionSlug)
}
func (p suggestionResolveParams) resolvedID() int64 {
	if p.ID != 0 {
		return p.ID
	}
	return p.SuggestionID
}

// HandleSuggestionResolve closes a suggestion with adopted/deferred/
// rejected. Mirrors HandleBugResolve in shape but enforces the
// suggestion-side vocabulary and rejects bug-side kinds (fixed/wontfix/
// etc.) with an explicit error naming the suggestion-side enum. Per
// chain `agent-suggestion-box` design, vocabulary enforcement lives here
// (not in a DB CHECK constraint) so the same row can be edited by older
// readers without schema-level conflicts.
func HandleSuggestionResolve(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (SuggestionResolveResult, error) {
	var p suggestionResolveParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return SuggestionResolveResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" {
		p.Slug = p.SuggestionSlug
	}
	if id := p.resolvedID(); p.Slug == "" && id > 0 {
		slug, err := lookupSuggestionSlugByID(ctx, pool, id)
		if err != nil {
			return SuggestionResolveResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return SuggestionResolveResult{Error: IdentifierRequiredError("suggestion_resolve")}, nil
	}
	rawKind := firstNonEmpty(p.ResolutionKind, p.Kind)
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	// Mirrors bug 1381's 'sha implies kind': supplying a SHA and omitting
	// kind defaults to 'adopted' — the dominant 'work shipped, here's the
	// commit' shape. deferred/rejected with a SHA is incoherent, so
	// disambiguation only matters when no SHA is present.
	if rawKind == "" && sha != "" {
		rawKind = "adopted"
	}
	if rawKind == "" {
		return SuggestionResolveResult{
			Error: "suggestion_resolve requires `resolution_kind` (or `kind` as alias) — one of: adopted, deferred, rejected (verb-form aliases also accepted: adopt, defer, reject). Pass a commit_sha to default kind=adopted without naming it explicitly.",
		}, nil
	}
	kind := normalizeSuggestionResolutionKind(rawKind)
	if kind != "adopted" && kind != "deferred" && kind != "rejected" {
		return SuggestionResolveResult{
			Error: fmt.Sprintf("invalid resolution_kind '%s', must be one of: adopted, deferred, rejected (suggestion vocabulary — NOT bug-side fixed/wontfix/upstream/dup/routed)", rawKind),
		}, nil
	}
	if sha != "" && !isValidCommitSHAOrSentinel(sha) {
		return SuggestionResolveResult{
			Error:  shaValidationError(sha, false),
			Action: "suggestion_resolve",
		}, nil
	}

	current, projectID, err := fetchSuggestionStatusAndProject(ctx, pool, p.Slug)
	if err != nil {
		return SuggestionResolveResult{Error: err.Error()}, nil
	}
	if current != "open" {
		return SuggestionResolveResult{
			Error: fmt.Sprintf("suggestion '%s' is in status '%s'; only open suggestions can be resolved (use suggestion_reopen to flip back to open first)", p.Slug, current),
		}, nil
	}

	emitPayload := events.SuggestionResolvedPayload{
		Kind:            kind,
		CommitSHA:       strPtr(sha),
		RoutedChainSlug: strPtr(p.RoutedChainSlug),
		RoutedTaskSlug:  strPtr(p.RoutedTaskSlug),
		RoutedBugSlug:   strPtr(p.RoutedBugSlug),
		ResolutionNote:  strPtr(firstNonEmpty(p.ResolutionNote, p.Notes, p.ResolutionSummary, p.Summary)),
	}

	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-suggestions: CRUD write dropped; projections/suggestions.go's
		// fold UPDATEs proj_current_suggestions from the event payload.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("suggestion", p.Slug, projectID),
			Payload: emitPayload,
		})
		return err
	})
	if err != nil {
		return SuggestionResolveResult{}, err
	}
	return SuggestionResolveResult{OK: true, Slug: p.Slug, Status: kind}, nil
}

// fetchSuggestionStatusAndProject returns status + project_id in one
// query. Callers that need project_id (event emit constructs an
// EntityRef with it) use this to avoid a second round-trip.
func fetchSuggestionStatusAndProject(ctx context.Context, pool *db.Pool, slug string) (string, string, error) {
	var status, projectID string
	err := pool.DB().QueryRowContext(ctx, `SELECT status, project_id FROM proj_current_suggestions WHERE slug = ?`, slug).Scan(&status, &projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("suggestion '%s' not found", slug)
		}
		return "", "", err
	}
	return status, projectID, nil
}

// fetchSuggestionResolutionSnapshot reads the current resolution-shape
// fields off a suggestion row. Used by HandleSuggestionReopen to populate
// the SuggestionReopenedPayload.PreviousResolution so the events ledger
// preserves the reversed resolution without forcing a walk back through
// events.
func fetchSuggestionResolutionSnapshot(ctx context.Context, pool *db.Pool, slug string) (events.SuggestionResolvedPayload, string, error) {
	var projectID, kind, routedChain, routedTask, routedBug, sha sql.NullString
	var statusCol string
	err := pool.DB().QueryRowContext(ctx,
		`SELECT project_id, status, resolution_kind,
		        routed_chain_slug, routed_task_slug, routed_bug_slug, resolved_commit_sha
		 FROM proj_current_suggestions WHERE slug = ?`, slug,
	).Scan(&projectID, &statusCol, &kind, &routedChain, &routedTask, &routedBug, &sha)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return events.SuggestionResolvedPayload{}, "", fmt.Errorf("suggestion '%s' not found", slug)
		}
		return events.SuggestionResolvedPayload{}, "", err
	}
	// resolution_note retired from the projection in migration 065
	// (Phase 4 F2). SuggestionReopened.previous_resolution.resolution_note
	// drops to nil; readers who want the original note follow the
	// SuggestionResolved event payload via the events ledger.
	payload := events.SuggestionResolvedPayload{Kind: kind.String}
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
	if routedBug.Valid && routedBug.String != "" {
		s := routedBug.String
		payload.RoutedBugSlug = &s
	}
	return payload, projectID.String, nil
}

// lookupSuggestionSlugByID resolves an id to a slug. Lets
// suggestion_resolve and suggestion_stamp_sha accept the same {id} alias
// suggestion_read supports; the rest of each handler still operates on
// the slug so the write SQL stays identical regardless of how the caller
// named the suggestion.
func lookupSuggestionSlugByID(ctx context.Context, pool *db.Pool, id int64) (string, error) {
	var slug string
	err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM proj_current_suggestions WHERE id = ?`, id).Scan(&slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("suggestion id %d not found", id)
		}
		return "", err
	}
	return slug, nil
}

// suggestionReopenParams + HandleSuggestionReopen flip a resolved
// suggestion back to open, clearing the resolution audit fields.
type suggestionReopenParams struct {
	Slug           string `json:"slug"`
	SuggestionSlug string `json:"suggestion_slug"`
	ID             int64  `json:"id"`
	SuggestionID   int64  `json:"suggestion_id"`
}

func HandleSuggestionReopen(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (SuggestionResolveResult, error) {
	var p suggestionReopenParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return SuggestionResolveResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" {
		p.Slug = p.SuggestionSlug
	}
	if id := firstNonZeroInt64(p.ID, p.SuggestionID); p.Slug == "" && id > 0 {
		slug, err := lookupSuggestionSlugByID(ctx, pool, id)
		if err != nil {
			return SuggestionResolveResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return SuggestionResolveResult{Error: IdentifierRequiredError("suggestion_reopen")}, nil
	}
	previousResolution, projectID, err := fetchSuggestionResolutionSnapshot(ctx, pool, p.Slug)
	if err != nil {
		return SuggestionResolveResult{Error: err.Error()}, nil
	}
	if previousResolution.Kind == "open" {
		return SuggestionResolveResult{Error: fmt.Sprintf("suggestion '%s' is already open", p.Slug)}, nil
	}
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-suggestions: CRUD write dropped; fold reopens proj_current_suggestions.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("suggestion", p.Slug, projectID),
			Payload: events.SuggestionReopenedPayload{PreviousResolution: previousResolution},
		})
		return err
	})
	if err != nil {
		return SuggestionResolveResult{}, err
	}
	return SuggestionResolveResult{OK: true, Slug: p.Slug, Status: "open"}, nil
}

// suggestionStampParams accepts slug + commit_sha (sha alias). `id`
// mirrors the bug 1329 alias on bug_stamp_sha so the suggestion_list →
// suggestion_stamp_sha flow can stay id-keyed end-to-end.
type suggestionStampParams struct {
	Slug           string `json:"slug"`
	SuggestionSlug string `json:"suggestion_slug"`
	ID             int64  `json:"id"`
	SuggestionID   int64  `json:"suggestion_id"`
	CommitSHA      string `json:"commit_sha"`
	SHA            string `json:"sha"`
}

// HandleSuggestionStampSHA stamps resolved_commit_sha on an
// already-resolved suggestion. Refuses to stamp open suggestions to keep
// the column meaningful — same shape as HandleBugStampSHA.
func HandleSuggestionStampSHA(ctx context.Context, pool *db.Pool, _ /*project*/ string, params json.RawMessage) (ShaStampResult, error) {
	var p suggestionStampParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ShaStampResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	sha := firstNonEmpty(p.CommitSHA, p.SHA)
	if p.Slug == "" {
		p.Slug = p.SuggestionSlug
	}
	if id := firstNonZeroInt64(p.ID, p.SuggestionID); p.Slug == "" && id > 0 {
		slug, err := lookupSuggestionSlugByID(ctx, pool, id)
		if err != nil {
			return ShaStampResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return ShaStampResult{Error: IdentifierRequiredError("suggestion_stamp_sha")}, nil
	}
	if sha == "" {
		return ShaStampResult{Error: "commit_sha must not be empty"}, nil
	}
	if !isValidCommitSHAOrSentinel(sha) {
		return ShaStampResult{
			Error:  shaValidationError(sha, false),
			Action: "suggestion_stamp_sha",
		}, nil
	}
	current, projectID, err := fetchSuggestionStatusAndProject(ctx, pool, p.Slug)
	if err != nil {
		return ShaStampResult{Error: err.Error()}, nil
	}
	if current == "open" {
		return ShaStampResult{
			Error: fmt.Sprintf("suggestion '%s' is still open; resolve it before stamping a SHA. Hint: suggestion_resolve accepts commit_sha directly — pass {resolution_kind: \"adopted\", commit_sha: \"<sha>\"} to resolve and stamp in one call.", p.Slug),
		}, nil
	}
	err = pool.WithWrite(ctx, func(tx *sql.Tx) error {
		// T5-suggestions: CRUD write dropped; fold stamps the SHA on proj_current_suggestions.
		_, err := events.Emit(ctx, tx, events.EmitArgs{
			Entity:  events.NewEntityRef("suggestion", p.Slug, projectID),
			Payload: events.SuggestionStampedPayload{CommitSHA: sha},
		})
		return err
	})
	if err != nil {
		return ShaStampResult{}, err
	}
	return ShaStampResult{OK: true, Slug: p.Slug, ResolvedCommitSHA: sha}, nil
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

var suggestionListDoc = ActionDoc{
	Purpose: "List suggestions. Compact projection by default; pass verbose=true for full bodies including problem_statement. Requires a project OR at least one filter; pass all=true to confirm a full-table dump.",
	Params: []DocParam{
		{Name: "status", Required: false, Description: "open / adopted / deferred / rejected. Alias: state."},
		{Name: "state", Required: false, Description: "Alias of status.", AliasOf: "status"},
		{Name: "priority", Required: false, Description: "low / medium / high."},
		{Name: "surface", Required: false, Description: "Comma-separated surface tags; match-any."},
		{Name: "since", Required: false, Description: "ISO timestamp; surfaces suggestions updated since."},
		{Name: "verbose", Required: false, Description: "Return full bodies including problem_statement."},
		{Name: "all", Required: false, Description: "Confirm a full-table dump when no project / filter passed."},
	},
	Example: `{"status":"open"}`,
	Errors: []ActionError{
		{Condition: "no project AND no filter", Message: "suggestion_list requires a project OR at least one filter (status, priority, surface, since); pass all=true to confirm a full-table dump."},
	},
}

var suggestionReadDoc = ActionDoc{
	Purpose: "Return a single suggestion's full content. Identify by id or slug.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Suggestion id (preferred)."},
		{Name: "slug", Required: false, Description: "Suggestion slug. Either id or slug identifies the suggestion (bug 1329 parity)."},
		{Name: "suggestion_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "suggestion_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
	},
	Example: `{"slug":"add-fts5-coverage-to-roadmap-list"}`,
	Notes:   "Numeric id is accepted in place of the slug; the dispatcher resolves either to the same row.",
}

var suggestionResolveDoc = ActionDoc{
	Purpose: "Resolve a suggestion (kind=adopted / deferred / rejected — distinct vocabulary from bug_resolve's fixed/wontfix/etc.). When commit_sha is supplied and kind is omitted, kind defaults to 'adopted'. For adopted with routing, supply routed_chain_slug + routed_task_slug (and routed_bug_slug when the adoption uncovers a concrete fix tracked as a bug).",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Suggestion id (preferred)."},
		{Name: "slug", Required: false, Description: "Suggestion slug. Either id or slug identifies the suggestion (bug 1329 parity)."},
		{Name: "suggestion_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "suggestion_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
		{Name: "resolution_kind", Required: false, Description: "adopted / deferred / rejected. Alias: kind. Defaults to 'adopted' when commit_sha is supplied; required otherwise."},
		{Name: "kind", Required: false, Description: "Alias of resolution_kind.", AliasOf: "resolution_kind"},
		{Name: "resolution_note", Required: false, Description: "Free-form note. For kind=adopted: what shipped (or the chain+task that will). For kind=deferred: why-not-now + revisit signal. For kind=rejected: reasoning. Aliases: notes, resolution_summary, summary."},
		{Name: "notes", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "resolution_summary", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "summary", Required: false, Description: "Alias of resolution_note.", AliasOf: "resolution_note"},
		{Name: "commit_sha", Required: false, Description: "SHA the work landed in. Accepts 'sha' as alias and 'unversioned' for artifacts outside any git repo. When set, defaults kind=adopted."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
		{Name: "routed_chain_slug", Required: false, Description: "When kind=adopted, the chain absorbing the work."},
		{Name: "routed_task_slug", Required: false, Description: "When kind=adopted, the specific task in routed_chain_slug."},
		{Name: "routed_bug_slug", Required: false, Description: "When kind=adopted and the adoption uncovers a concrete fix tracked as a bug, the bug slug. Symmetric counterpart to bug.routed_suggestion_slug — bidirectional bug↔suggestion routing."},
	},
	Example: `{"slug":"add-fts5-coverage-to-roadmap-list","kind":"adopted","routed_chain_slug":"fts5-on-roadmap-list"}`,
	ValueAliases: []ActionValueAlias{
		{Param: "resolution_kind", From: "adopt", To: "adopted"},
		{Param: "resolution_kind", From: "defer", To: "deferred"},
		{Param: "resolution_kind", From: "reject", To: "rejected"},
	},
	Errors: []ActionError{
		{Condition: "missing resolution_kind (and no commit_sha to default from)", Message: "Returns an explicit error naming all three accepted values."},
		{Condition: "bug-side kind supplied (fixed, wontfix, upstream, dup, routed)", Message: "Rejected with an error naming the suggestion-side vocabulary — adopted/deferred/rejected."},
	},
	Notes:                "The suggestion-side vocabulary (adopted/deferred/rejected) is deliberately distinct from the bug-side (fixed/wontfix/upstream/dup/routed). adopted + routed_chain_slug + resolved_commit_sha is the canonical 'this shipped' shape — there is no separate `implemented` kind. routed_bug_slug exists for the bidirectional bug↔suggestion routing introduced in chain agent-suggestion-box (a suggestion's adoption can name the bug filed for the implementing work).",
	EnvelopeRequirements: rationaleEnv(),
}

var suggestionReopenDoc = ActionDoc{
	Purpose: "Re-open a previously-resolved suggestion.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Suggestion id (preferred)."},
		{Name: "slug", Required: false, Description: "Suggestion slug. Either id or slug identifies the suggestion (bug 1329 parity)."},
		{Name: "suggestion_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "suggestion_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
	},
	Example:              `{"slug":"add-fts5-coverage-to-roadmap-list"}`,
	EnvelopeRequirements: rationaleEnv(),
}

var suggestionStampShaDoc = ActionDoc{
	Purpose: "Stamp a commit SHA on an already-resolved suggestion — for when the implementing work landed after suggestion_resolve. Mirrors bug_stamp_sha / task_stamp_sha.",
	Params: []DocParam{
		{Name: "id", Required: false, Description: "Suggestion id (preferred)."},
		{Name: "slug", Required: false, Description: "Suggestion slug. Either id or slug identifies the suggestion (bug 1329 parity)."},
		{Name: "suggestion_slug", Required: false, Description: "Alias of slug.", AliasOf: "slug"},
		{Name: "suggestion_id", Required: false, Description: "Alias of id.", AliasOf: "id"},
		{Name: "commit_sha", Required: true, Description: "SHA to stamp. Alias: sha. Accepts 'unversioned'."},
		{Name: "sha", Required: false, Description: "Alias of commit_sha.", AliasOf: "commit_sha"},
	},
	Example:              `{"slug":"add-fts5-coverage-to-roadmap-list","sha":"abc1234"}`,
	EnvelopeRequirements: rationaleEnv(),
}
