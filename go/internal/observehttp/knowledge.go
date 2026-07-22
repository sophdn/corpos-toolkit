package observehttp

import (
	"context"
	"database/sql"
	"net/http"

	tkdb "toolkit/internal/db"
)

type sourceTypeCount struct {
	SourceType string `json:"source_type"`
	Count      int64  `json:"count"`
}

type topPointer struct {
	ID         int64  `json:"id"`
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`
	Question   string `json:"question"`
	UsageCount int64  `json:"usage_count"`
}

type recentPointer struct {
	ID         int64  `json:"id"`
	SourceType string `json:"source_type"`
	SourceRef  string `json:"source_ref"`
	Question   string `json:"question"`
	CreatedAt  string `json:"created_at"`
}

type groundingSummary struct {
	TotalSearchCalls   int64   `json:"total_search_calls"`
	UsedCount          int64   `json:"used_count"`
	UsedPct            float64 `json:"used_pct"`
	ZeroResultGapCount int64   `json:"zero_result_gap_count"`
	PureMemorySessions int64   `json:"pure_memory_sessions"`
}

type knowledgeIndexCard struct {
	TotalActivePointers       int64             `json:"total_active_pointers"`
	BySourceType              []sourceTypeCount `json:"by_source_type"`
	PendingCurationCandidates int64             `json:"pending_curation_candidates"`
	TopQueried                []topPointer      `json:"top_queried"`
	RecentlyAdded             []recentPointer   `json:"recently_added"`
	GroundingSummary          groundingSummary  `json:"grounding_summary"`
}

func (s AppState) knowledgeIndexCard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	project := projectFilter(r)
	db := s.Pool.DB()

	scalar := func(unscoped, scoped string) (int64, error) {
		var n int64
		var err error
		if project != "" {
			err = db.QueryRowContext(ctx, scoped, project).Scan(&n)
		} else {
			err = db.QueryRowContext(ctx, unscoped).Scan(&n)
		}
		return n, err
	}

	totalActive, err := scalar(
		`SELECT COUNT(*) FROM knowledge_pointers WHERE status='active'`,
		`SELECT COUNT(*) FROM knowledge_pointers WHERE status='active' AND project_id=?`,
	)
	if err != nil {
		dbErr(w, err)
		return
	}

	bySourceType, err := knowledgeBySourceType(ctx, db, project)
	if err != nil {
		dbErr(w, err)
		return
	}

	pending, err := scalar(
		`SELECT COUNT(*) FROM curation_candidates WHERE status='pending'`,
		`SELECT COUNT(*) FROM curation_candidates WHERE status='pending' AND project_id=?`,
	)
	if err != nil {
		dbErr(w, err)
		return
	}

	top, err := knowledgeTopQueried(ctx, db, project)
	if err != nil {
		dbErr(w, err)
		return
	}

	recent, err := knowledgeRecentlyAdded(ctx, db, project)
	if err != nil {
		dbErr(w, err)
		return
	}

	totalSearch, err := scalarUnscoped(ctx, db,
		`SELECT COUNT(*) FROM grounding_events WHERE action='knowledge_search'`)
	if err != nil {
		dbErr(w, err)
		return
	}
	used, err := scalarUnscoped(ctx, db,
		`SELECT COUNT(*) FROM grounding_events WHERE action='knowledge_search' AND used=1`)
	if err != nil {
		dbErr(w, err)
		return
	}
	gap, err := scalarUnscoped(ctx, db,
		`SELECT COUNT(*) FROM grounding_events
		 WHERE action='knowledge_search' AND results_count=0 AND next_turn_has_output=1`)
	if err != nil {
		dbErr(w, err)
		return
	}
	pureMem, err := scalarUnscoped(ctx, db,
		`SELECT COUNT(DISTINCT session_id)
		 FROM grounding_events
		 WHERE next_turn_has_output=1
		   AND session_id NOT IN (
		     SELECT DISTINCT session_id FROM grounding_events
		     WHERE action='knowledge_search'
		   )`)
	if err != nil {
		dbErr(w, err)
		return
	}

	usedPct := 0.0
	if totalSearch > 0 {
		usedPct = float64(used) / float64(totalSearch) * 100.0
	}

	writeJSON(w, http.StatusOK, knowledgeIndexCard{
		TotalActivePointers:       totalActive,
		BySourceType:              bySourceType,
		PendingCurationCandidates: pending,
		TopQueried:                top,
		RecentlyAdded:             recent,
		GroundingSummary: groundingSummary{
			TotalSearchCalls:   totalSearch,
			UsedCount:          used,
			UsedPct:            usedPct,
			ZeroResultGapCount: gap,
			PureMemorySessions: pureMem,
		},
	})
}

func scalarUnscoped(ctx context.Context, db *sql.DB, q string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

func knowledgeBySourceType(ctx context.Context, db *sql.DB, project string) ([]sourceTypeCount, error) {
	q := `SELECT source_type, COUNT(*) AS count
	      FROM knowledge_pointers WHERE status='active'`
	binds := tkdb.NewArgs()
	if project != "" {
		q += " AND project_id=?"
		binds.AddString(project)
	}
	q += " GROUP BY source_type ORDER BY count DESC"
	rows, err := db.QueryContext(ctx, q, binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sourceTypeCount{}
	for rows.Next() {
		var s sourceTypeCount
		if err := rows.Scan(&s.SourceType, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func knowledgeTopQueried(ctx context.Context, db *sql.DB, project string) ([]topPointer, error) {
	q := `SELECT id, source_type, source_ref, question, usage_count
	      FROM knowledge_pointers WHERE status='active'`
	binds := tkdb.NewArgs()
	if project != "" {
		q += " AND project_id=?"
		binds.AddString(project)
	}
	q += " ORDER BY usage_count DESC LIMIT 5"
	rows, err := db.QueryContext(ctx, q, binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []topPointer{}
	for rows.Next() {
		var p topPointer
		if err := rows.Scan(&p.ID, &p.SourceType, &p.SourceRef, &p.Question, &p.UsageCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func knowledgeRecentlyAdded(ctx context.Context, db *sql.DB, project string) ([]recentPointer, error) {
	q := `SELECT id, source_type, source_ref, question, created_at
	      FROM knowledge_pointers WHERE status='active'
	        AND created_at >= datetime('now', '-7 days')`
	binds := tkdb.NewArgs()
	if project != "" {
		q += " AND project_id=?"
		binds.AddString(project)
	}
	q += " ORDER BY created_at DESC LIMIT 20"
	rows, err := db.QueryContext(ctx, q, binds.Slice()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recentPointer{}
	for rows.Next() {
		var p recentPointer
		if err := rows.Scan(&p.ID, &p.SourceType, &p.SourceRef, &p.Question, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
