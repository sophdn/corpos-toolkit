package work

// recent.go implements work.recent_activity (alias: where_we_left_off) —
// the "where did we leave off" resume briefing.
//
// Why this exists: chain_status (no slug) returns OPEN chains only, so it
// is structurally unable to answer "what happened recently" — closed
// chains and landed commits are invisible to it, and the rest of the work
// surface has no cross-status, time-ordered view either (task_search needs
// a chain/pattern; only bug_list / suggestion_list accept `since`). The
// data was always present in the append-only `events` ledger (migration
// 032); recent_activity is the action that finally reads it as a timeline.
//
// The briefing is deliberately cross-status: it reads the ledger directly,
// so a closed chain, a completed task, and a landed commit all surface
// the same way. It is read-only (no rationale gate) and scopes per-project
// via the events_project_ts_idx (entity_project_id, ts) index, or
// cross-project when no project is supplied.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"toolkit/internal/db"
	"toolkit/internal/dbutil"
)

// recentActivityParams captures the resume-briefing filters. All three are
// optional: with nothing supplied the briefing covers the last 7 days,
// newest first, capped at the default limit.
type recentActivityParams struct {
	// Since is an explicit ISO-8601 lower bound on event ts. When set it
	// overrides Days. Compared lexically against events.ts (RFC-3339), so
	// an ISO string in the same shape as the ledger sorts correctly.
	Since string `json:"since"`
	// Days is a convenience window: events from the last N days. Ignored
	// when Since is set. Defaults to 7 when neither is supplied.
	Days int64 `json:"days"`
	// Limit caps the timeline and recent-closures sections (each). The
	// in-flight section is naturally small and is not capped.
	Limit int64 `json:"limit"`
}

// ActivityEvent is one ledger row rendered for the briefing: a timestamp,
// the event type, the entity it touched, who did it, and a one-line
// human summary derived from the type + payload + rationale.
type ActivityEvent struct {
	TS         string `json:"ts"`
	Type       string `json:"type"`
	EntityKind string `json:"entity_kind"`
	EntitySlug string `json:"entity_slug"`
	Project    string `json:"project,omitempty"`
	Actor      string `json:"actor"`
	Summary    string `json:"summary"`
}

// InFlightTask is a currently-active task plus the chain it belongs to —
// the "what is literally mid-stream right now" half of the briefing.
type InFlightTask struct {
	ChainSlug        string `json:"chain_slug"`
	Slug             string `json:"slug"`
	Status           string `json:"status"`
	Project          string `json:"project,omitempty"`
	UpdatedAt        string `json:"updated_at"`
	ProblemStatement string `json:"problem_statement,omitempty"`
}

// RecentActivityResult is the resume briefing: what's mid-stream, what
// recently closed, and the raw recent timeline beneath both.
type RecentActivityResult struct {
	Scope          string          `json:"scope"`
	Since          string          `json:"since"`
	InFlight       []InFlightTask  `json:"in_flight"`
	RecentClosures []ActivityEvent `json:"recent_closures"`
	Timeline       []ActivityEvent `json:"timeline"`
}

// closureEventTypes are the lifecycle events that mean "work finished" —
// the class chain_status hides. Surfaced as their own briefing section so
// closed work is never invisible.
var closureEventTypes = []string{"ChainClosed", "TaskCompleted", "TaskCancelled"}

// HandleRecentActivity implements work.recent_activity. Cross-project when
// project is empty (mirrors roadmap_list / chain_status / bug_list); pass a
// top-level project to scope. Read-only.
func HandleRecentActivity(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (RecentActivityResult, error) {
	var p recentActivityParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return RecentActivityResult{}, fmt.Errorf("parse params: %w", err)
		}
	}

	limit, _ := normalizeLimitOffset(p.Limit, 0, 30)
	since := effectiveSince(p.Since, p.Days)

	scope := "cross-project"
	if project != "" {
		scope = project
	}

	result := RecentActivityResult{
		Scope:          scope,
		Since:          since,
		InFlight:       []InFlightTask{},
		RecentClosures: []ActivityEvent{},
		Timeline:       []ActivityEvent{},
	}

	timeline, err := queryEvents(ctx, pool, project, since, nil, limit)
	if err != nil {
		return RecentActivityResult{}, err
	}
	result.Timeline = timeline

	closures, err := queryEvents(ctx, pool, project, since, closureEventTypes, limit)
	if err != nil {
		return RecentActivityResult{}, err
	}
	result.RecentClosures = closures

	inFlight, err := queryInFlight(ctx, pool, project)
	if err != nil {
		return RecentActivityResult{}, err
	}
	result.InFlight = inFlight

	return result, nil
}

// effectiveSince resolves the lower-bound timestamp. An explicit ISO
// `since` wins; otherwise it's now minus `days` (default 7). The bound is
// rendered in the same RFC-3339 millisecond-UTC shape the ledger uses so
// the `ts >= ?` comparison is a correct lexical compare.
func effectiveSince(sinceArg string, days int64) string {
	if sinceArg != "" {
		return sinceArg
	}
	if days <= 0 {
		days = 7
	}
	return time.Now().UTC().AddDate(0, 0, -int(days)).Format("2006-01-02T15:04:05.000Z")
}

// queryEvents reads the events ledger newest-first within the window,
// optionally filtered to a set of event types. Per-project scope rides the
// events_project_ts_idx (entity_project_id, ts); cross-project drops the
// project predicate. Each row is rendered into an ActivityEvent with a
// one-line summary.
func queryEvents(ctx context.Context, pool *db.Pool, project, since string, types []string, limit int64) ([]ActivityEvent, error) {
	wb := dbutil.NewWhereBuilder().
		Eq("entity_project_id", project).
		GtEqString("ts", since)
	whereClause := wb.Clause()
	args := wb.Args().Slice()

	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, t := range types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		typeClause := "type IN (" + strings.Join(placeholders, ", ") + ")"
		if whereClause == "" {
			whereClause = "WHERE " + typeClause
		} else {
			whereClause += " AND " + typeClause
		}
	}

	limitClause := ""
	if limit >= 0 {
		limitClause = fmt.Sprintf("LIMIT %d", limit)
	}

	query := fmt.Sprintf(`SELECT ts, type, entity_kind, entity_slug, entity_project_id,
		actor_kind, actor_id, payload, rationale
		FROM events %s ORDER BY ts DESC %s`, whereClause, limitClause)
	rows, err := pool.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ActivityEvent{}
	for rows.Next() {
		var e ActivityEvent
		var projectID, actorKind, actorID, rationale sql.NullString
		var payload []byte
		if err := rows.Scan(&e.TS, &e.Type, &e.EntityKind, &e.EntitySlug, &projectID,
			&actorKind, &actorID, &payload, &rationale); err != nil {
			return nil, err
		}
		e.Project = projectID.String
		e.Actor = formatActor(actorKind.String, actorID.String)
		e.Summary = summarizeEvent(e.Type, e.EntityKind, e.EntitySlug, payload, rationale.String)
		out = append(out, e)
	}
	return out, rows.Err()
}

// queryInFlight reads the currently-active tasks and the chains they belong
// to from the task projection. Scoped to project when supplied.
func queryInFlight(ctx context.Context, pool *db.Pool, project string) ([]InFlightTask, error) {
	const base = `SELECT c.slug, t.slug, t.status, c.project_id, t.updated_at, t.problem_statement
		FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		WHERE t.status = 'active'`
	var rows *sql.Rows
	var err error
	if project != "" {
		rows, err = pool.DB().QueryContext(ctx, base+` AND c.project_id = ? ORDER BY t.updated_at DESC`, project)
	} else {
		rows, err = pool.DB().QueryContext(ctx, base+` ORDER BY t.updated_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []InFlightTask{}
	for rows.Next() {
		var it InFlightTask
		var problem string
		if err := rows.Scan(&it.ChainSlug, &it.Slug, &it.Status, &it.Project, &it.UpdatedAt, &problem); err != nil {
			return nil, err
		}
		it.ProblemStatement = truncate(problem, 160)
		out = append(out, it)
	}
	return out, rows.Err()
}

// formatActor renders the event's actor as "kind:id", collapsing to just
// the kind when the id is empty or redundant.
func formatActor(kind, id string) string {
	switch {
	case kind == "" && id == "":
		return ""
	case id == "" || id == kind:
		return kind
	case kind == "":
		return id
	default:
		return kind + ":" + id
	}
}

// summarizeEvent renders one ledger row as a readable line. It humanizes
// the PascalCase event type for a generic-but-meaningful summary (so
// unknown / future event types still read sensibly), with a special case
// for CommitLanded which carries a human subject line worth surfacing.
// A non-empty rationale is appended (truncated) since it's the "why".
func summarizeEvent(evType, kind, slug string, payload json.RawMessage, rationale string) string {
	if evType == "CommitLanded" {
		if s := commitLandedSummary(payload); s != "" {
			return s
		}
	}

	summary := humanizeEventType(evType)
	if slug != "" {
		if kind != "" {
			summary += fmt.Sprintf(" %s '%s'", kind, slug)
		} else {
			summary += fmt.Sprintf(" '%s'", slug)
		}
	}
	if rationale != "" {
		summary += " — " + truncate(rationale, 140)
	}
	return summary
}

// commitLandedSummary pulls the subject + short SHA off a CommitLanded
// payload. Returns "" when the payload can't be read so the caller falls
// back to the generic summary.
func commitLandedSummary(payload json.RawMessage) string {
	var p struct {
		Subject   string `json:"subject"`
		CommitSHA string `json:"commit_sha"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &p) != nil {
		return ""
	}
	short := p.CommitSHA
	if len(short) > 8 {
		short = short[:8]
	}
	switch {
	case p.Subject != "" && short != "":
		return fmt.Sprintf("commit %s landed: %s", short, p.Subject)
	case p.Subject != "":
		return "commit landed: " + p.Subject
	case short != "":
		return "commit " + short + " landed"
	default:
		return ""
	}
}

// humanizeEventType turns "TaskCompleted" into "task completed" by
// splitting on interior capitals. Gives every event type — including ones
// added after this code ships — a readable summary with no per-type table.
func humanizeEventType(s string) string {
	if s == "" {
		return "event"
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte(' ')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// truncate clips s to max runes, appending an ellipsis when it cut.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ") + "…"
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

var recentActivityDoc = ActionDoc{
	Purpose: "Resume briefing — answers \"where did we leave off?\". Reads the append-only events ledger as a time-ordered timeline (cross-project by default; pass `project` to scope), plus an in_flight section (currently-active tasks) and a recent_closures section (chains closed / tasks completed). Cross-status by construction, so closed chains and landed commits are visible — unlike chain_status, which lists only open chains and cannot answer recency.",
	Params: []DocParam{
		{Name: "since", Required: false, Description: "ISO-8601 lower bound on event ts. Overrides `days`. Compared lexically against the ledger's RFC-3339 timestamps."},
		{Name: "days", Required: false, Description: "Convenience window: events from the last N days. Ignored when `since` is set. Defaults to 7."},
		{Name: "limit", Required: false, Description: "Caps the timeline and recent_closures sections (each). Default 30."},
	},
	Example: `{"days":2}`,
	Notes: "Read-only — no rationale required. Scope is CROSS-PROJECT by default; pass a top-level `project` to scope to one project.\n\n" +
		"This is the durable fix for the open-only chain_status recency gap: chain_status (no slug) returns only OPEN chains, so its newest entry is not the newest activity — closed chains and CommitLanded events are absent. recent_activity reads the events ledger directly (migration 032), so every lifecycle event is in scope.\n\n" +
		"Sections: `in_flight` = active tasks + their chains (what's mid-stream now); `recent_closures` = ChainClosed / TaskCompleted / TaskCancelled in the window; `timeline` = the raw recent events, newest first, each with a one-line summary derived from the event type + payload + rationale.",
	SeeAlso: "chain_status, roadmap_list",
}

var whereWeLeftOffDoc = ActionDoc{
	Purpose: "Alias of recent_activity — both names route to the same resume-briefing handler.",
}
