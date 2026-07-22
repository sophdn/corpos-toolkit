package refresolve

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"toolkit/internal/db"
)

// Work-state surfacing caps per chain parse-context-lean-orienting T6
// acceptance criteria. The envelope is a triage surface, not a
// dashboard — these caps bound the surface so an agent reading it
// scans rather than wades.
//
// Tightened by bug 866 fix (chain parse-context-lean-orienting T10
// retrospective; envelope p50 had grown from 719 B → 4,076 B
// post-T9, with work-state dominating ~4.5 KB per work-shape call).
// New caps halve the envelope while preserving the triage-set
// invariant: the agent sees the highest-severity / most-recent
// items first and can task_list / bug_list directly for the long
// tail.
const (
	workStateMaxBugs          = 5
	workStateMaxTasks         = 3
	workStateMaxChains        = 2
	workStateChainsRecentDays = 7
)

// workStateFiringIntents is the closed set of intent shapes that
// unlock work-state surfacing. T6 §"Fires on intent ∈ ..." names the
// shapes; docs-only intents (explain, summarize) stay quiet because
// work-state isn't load-bearing for those.
var workStateFiringIntents = map[IntentShape]bool{
	IntentVerify:    true,
	IntentImplement: true,
	IntentFix:       true,
	IntentAudit:     true,
	IntentStatus:    true,
	IntentList:      true,
	// IntentExecute (chain parse-context-directive-intent-extension,
	// §14.4): execute is the dominant directive. For a no-slug prompt
	// ("clear the backlog", "pick up the next ready task") the open
	// bugs/active tasks ARE the work, so work-state is the primary
	// surface. For a named-slug prompt the chain is already surfaced by
	// slug detection; surfaceWorkState dedups the duplicate so the
	// chain isn't double-listed.
	IntentExecute: true,
}

// WorkStateTelemetry is the per-call summary the parse_context handler
// stamps onto the ParseContextWorkStateSurfaced event. Distinct from
// the resolved ResolvedReference slice so the event emit doesn't have
// to re-walk the list to count categories.
type WorkStateTelemetry struct {
	IntentShape string
	BugsCount   int
	TasksCount  int
	ChainsCount int
	CacheHit    bool
	ProjectID   string
}

// ResolveWorkState runs the three work-state queries (open bugs,
// active tasks, recent open chains), caps each, and returns one
// ResolvedReference per surfaced item plus the per-call telemetry.
//
// Project-scoped: when project is empty the resolver returns no
// surfacings (the constraint from T6 §"no-project case"). Cross-
// project surfacing is out of scope for the v1; the design names it
// as a follow-on guarded by an explicit intent qualifier ("list all
// open bugs everywhere") that the §13.2 vocabulary doesn't carry.
//
// No-fire intents (explain, summarize, none) short-circuit to
// empty-result with empty telemetry — the caller distinguishes
// "didn't fire" from "fired and found nothing" via the telemetry
// IntentShape field (empty when the resolver short-circuited).
func ResolveWorkState(ctx context.Context, pool *db.Pool, project string, intent IntentShape, cache *WorkStateCache, sessionID string) ([]ResolvedReference, WorkStateTelemetry, error) {
	if pool == nil || project == "" || !workStateFiringIntents[intent] {
		return nil, WorkStateTelemetry{}, nil
	}
	tel := WorkStateTelemetry{
		IntentShape: string(intent),
		ProjectID:   project,
	}
	// Cache lookup: (sessionID, project, intent) keyed; short-5-turns
	// TTL. The cache itself handles the freshness check.
	if cache != nil {
		if cached, ok := cache.Get(sessionID, project, intent); ok {
			tel.CacheHit = true
			tel.BugsCount = cached.BugCount
			tel.TasksCount = cached.TaskCount
			tel.ChainsCount = cached.ChainCount
			return cached.Refs, tel, nil
		}
	}

	out := []ResolvedReference{}

	bugs, err := queryOpenBugs(ctx, pool.DB(), project, workStateMaxBugs)
	if err != nil {
		return nil, tel, fmt.Errorf("open bugs: %w", err)
	}
	for _, b := range bugs {
		out = append(out, workStateBugRef(b, project))
	}
	tel.BugsCount = len(bugs)

	tasks, err := queryActiveTasks(ctx, pool.DB(), project, workStateMaxTasks)
	if err != nil {
		return nil, tel, fmt.Errorf("active tasks: %w", err)
	}
	for _, t := range tasks {
		out = append(out, workStateTaskRef(t))
	}
	tel.TasksCount = len(tasks)

	chains, err := queryRecentOpenChains(ctx, pool.DB(), project, workStateMaxChains, workStateChainsRecentDays)
	if err != nil {
		return nil, tel, fmt.Errorf("recent chains: %w", err)
	}
	for _, c := range chains {
		out = append(out, workStateChainRef(c))
	}
	tel.ChainsCount = len(chains)

	if cache != nil {
		cache.Put(sessionID, project, intent, workStateCacheEntry{
			Refs:       out,
			BugCount:   tel.BugsCount,
			TaskCount:  tel.TasksCount,
			ChainCount: tel.ChainsCount,
		})
	}
	return out, tel, nil
}

// workStateBug is the minimal row shape ResolveWorkState reads from
// proj_current_bugs — slug, title, severity, status are the fields
// the envelope's PresentedAs / DebugNotes consume.
type workStateBug struct {
	Slug     string
	Title    string
	Severity string
}

func queryOpenBugs(ctx context.Context, db *sql.DB, project string, cap int) ([]workStateBug, error) {
	// Ordering: severity desc (high > medium > low) then filed_at desc
	// per T6 acceptance criteria. SQLite lacks a real enum so we map
	// severity strings to ranks inline via CASE.
	query := `
		SELECT slug, title, severity
		FROM proj_current_bugs
		WHERE project_id = ?
		  AND status IN ('open', 'reopened')
		ORDER BY
			CASE severity WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END,
			filed_at DESC
		LIMIT ?`
	rows, err := db.QueryContext(ctx, query, project, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workStateBug{}
	for rows.Next() {
		var b workStateBug
		if err := rows.Scan(&b.Slug, &b.Title, &b.Severity); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type workStateTask struct {
	Slug      string
	Title     string
	ChainSlug string
	UpdatedAt string
}

func queryActiveTasks(ctx context.Context, db *sql.DB, project string, cap int) ([]workStateTask, error) {
	// proj_current_tasks rows are keyed on chain_id; join through
	// proj_chain_status to scope by project_id. Title is the
	// problem_statement's first line for compact display.
	query := `
		SELECT t.slug, substr(t.problem_statement, 1, 100), c.slug, t.updated_at
		FROM proj_current_tasks t
		JOIN proj_chain_status c ON c.id = t.chain_id
		WHERE c.project_id = ?
		  AND t.status = 'active'
		ORDER BY t.updated_at DESC
		LIMIT ?`
	rows, err := db.QueryContext(ctx, query, project, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workStateTask{}
	for rows.Next() {
		var t workStateTask
		if err := rows.Scan(&t.Slug, &t.Title, &t.ChainSlug, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type workStateChain struct {
	Slug      string
	Output    string
	Pending   int
	Active    int
	Blocked   int
	UpdatedAt string
}

func queryRecentOpenChains(ctx context.Context, db *sql.DB, project string, cap, recentDays int) ([]workStateChain, error) {
	// Recent = updated_at within the last recentDays. SQLite's
	// datetime('now', '-7 days') is the recommended comparison form;
	// the column is ISO-8601 text so string compare works without a
	// CAST.
	query := fmt.Sprintf(`
		SELECT slug, output, pending, active, blocked, updated_at
		FROM proj_chain_status
		WHERE project_id = ?
		  AND status = 'open'
		  AND updated_at >= datetime('now', '-%d days')
		ORDER BY updated_at DESC
		LIMIT ?`, recentDays)
	rows, err := db.QueryContext(ctx, query, project, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workStateChain{}
	for rows.Next() {
		var c workStateChain
		if err := rows.Scan(&c.Slug, &c.Output, &c.Pending, &c.Active, &c.Blocked, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// workStateBugRef composes a ResolvedReference surface for one open
// bug. RecommendedAction is "mention_as_possibly_relevant" — these
// surfacings aren't message-resolved tokens, they're proactive
// suggestions for triage.
func workStateBugRef(b workStateBug, project string) ResolvedReference {
	return ResolvedReference{
		Token:             b.Slug,
		Shape:             ShapeBugSlug,
		ConfidenceTier:    TierSingleExact,
		PresentedAs:       fmt.Sprintf("[work-state surface] open bug `%s` (severity=%s) — %s", b.Slug, b.Severity, b.Title),
		RecommendedAction: PresentMentionAsPossiblyRelevant,
		CachePolicy:       string(PolicyShortFiveTurns),
		TopCandidates: []Candidate{{
			ID:         b.Slug,
			Title:      b.Title,
			Score:      1.0,
			SourceRef:  fmt.Sprintf("bug:%s/%s", project, b.Slug),
			DebugNotes: fmt.Sprintf("severity=%s source=work-state", b.Severity),
		}},
	}
}

func workStateTaskRef(t workStateTask) ResolvedReference {
	return ResolvedReference{
		Token:             t.Slug,
		Shape:             ShapeTaskSlug,
		ConfidenceTier:    TierSingleExact,
		PresentedAs:       fmt.Sprintf("[work-state surface] active task `%s` on chain `%s` — %s", t.Slug, t.ChainSlug, t.Title),
		RecommendedAction: PresentMentionAsPossiblyRelevant,
		CachePolicy:       string(PolicyShortFiveTurns),
		TopCandidates: []Candidate{{
			ID:         t.Slug,
			Title:      t.Title,
			Score:      1.0,
			SourceRef:  fmt.Sprintf("task:%s/%s", t.ChainSlug, t.Slug),
			DebugNotes: fmt.Sprintf("status=active chain=%s updated_at=%s source=work-state", t.ChainSlug, t.UpdatedAt),
		}},
	}
}

func workStateChainRef(c workStateChain) ResolvedReference {
	summary := c.Output
	if len(summary) > 140 {
		summary = summary[:140] + "…"
	}
	return ResolvedReference{
		Token:             c.Slug,
		Shape:             ShapeChainSlug,
		ConfidenceTier:    TierSingleExact,
		PresentedAs:       fmt.Sprintf("[work-state surface] recent chain `%s` (pending=%d active=%d blocked=%d) — %s", c.Slug, c.Pending, c.Active, c.Blocked, summary),
		RecommendedAction: PresentMentionAsPossiblyRelevant,
		CachePolicy:       string(PolicyShortFiveTurns),
		TopCandidates: []Candidate{{
			ID:         c.Slug,
			Title:      summary,
			Score:      1.0,
			SourceRef:  fmt.Sprintf("chain:%s", c.Slug),
			DebugNotes: fmt.Sprintf("status=open pending=%d active=%d blocked=%d updated_at=%s source=work-state", c.Pending, c.Active, c.Blocked, c.UpdatedAt),
		}},
	}
}

// WorkStateCache holds resolved work-state surfacings per
// (sessionID, project, intent) with the design's short-5-turns TTL.
// Distinct from ParseContextCache (which is token-keyed) — the
// work-state surface is a per-query bundle, not a single token's
// candidates.
//
// Invalidation: the events fold hook in cache_invalidate.go drops
// every WorkStateCache entry on any cacheable-entity event
// (chain/task/bug emits) so subsequent calls re-query against the
// fresh state. Cross-session sweep mirrors the ParseContextCache
// invariant.
type WorkStateCache struct {
	mu    sync.RWMutex
	byKey map[workStateCacheKey]workStateCacheCachedEntry
}

type workStateCacheKey struct {
	SessionID string
	Project   string
	Intent    IntentShape
}

type workStateCacheEntry struct {
	Refs       []ResolvedReference
	BugCount   int
	TaskCount  int
	ChainCount int
}

type workStateCacheCachedEntry struct {
	Entry    workStateCacheEntry
	CachedAt time.Time
}

// NewWorkStateCache builds an empty cache ready for use.
func NewWorkStateCache() *WorkStateCache {
	return &WorkStateCache{byKey: make(map[workStateCacheKey]workStateCacheCachedEntry)}
}

// Get returns the cached entry for the key if one exists and the
// short-5-turns TTL still admits it.
func (c *WorkStateCache) Get(sessionID, project string, intent IntentShape) (workStateCacheEntry, bool) {
	if c == nil || sessionID == "" {
		return workStateCacheEntry{}, false
	}
	c.mu.RLock()
	cached, ok := c.byKey[workStateCacheKey{sessionID, project, intent}]
	c.mu.RUnlock()
	if !ok {
		return workStateCacheEntry{}, false
	}
	if time.Since(cached.CachedAt) >= shortFiveTurnsTTL {
		return workStateCacheEntry{}, false
	}
	return cached.Entry, true
}

// Put stores a fresh entry under (sessionID, project, intent).
func (c *WorkStateCache) Put(sessionID, project string, intent IntentShape, entry workStateCacheEntry) {
	if c == nil || sessionID == "" {
		return
	}
	c.mu.Lock()
	c.byKey[workStateCacheKey{sessionID, project, intent}] = workStateCacheCachedEntry{
		Entry:    entry,
		CachedAt: time.Now(),
	}
	c.mu.Unlock()
}

// InvalidateAll drops every entry. The events fold hook calls this
// on any cacheable-entity emit (chain/task/bug) — the surface
// summarizes ALL work state, so any state mutation potentially
// changes any cached entry. Coarse-but-correct.
func (c *WorkStateCache) InvalidateAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.byKey = make(map[workStateCacheKey]workStateCacheCachedEntry)
	c.mu.Unlock()
}

// Len returns the count of live entries. Test-only ergonomics.
func (c *WorkStateCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byKey)
}
