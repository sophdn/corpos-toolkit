package observehttp

import (
	"net/http"
	"strconv"
)

// parseContextSkipRateResponse is the wire shape for the skip-rate
// visibility surface (bug 1452). One window snapshot of how many
// observed user prompts (rows with non-empty prompt_id from the
// post-session Stop-hook processor) ALSO had a parse_context /
// resolve_references grounding_events row land.
//
// SkipRate is 1 - (WithParseContext / ObservedPrompts). When
// ObservedPrompts is zero, SkipRate is 0 — there's nothing to skip.
// The dashboard / operator queries this to know whether the
// parse-context-first-call reflex is firing in practice.
type parseContextSkipRateResponse struct {
	WindowHours      int      `json:"window_hours"`
	ObservedPrompts  int64    `json:"observed_prompts"`
	WithParseContext int64    `json:"with_parse_context"`
	SkipRate         float64  `json:"skip_rate"`
	SkippedPromptIDs []string `json:"skipped_prompt_ids,omitempty"`
}

// parseContextSkipRate is the GET /admin/parse-context-skip-rate
// handler — the structural visibility surface for bug 1452. The bug
// observed agent-side reflex compliance drift (parse_context skipped
// across many prompts without an in-session signal); this endpoint is
// the after-the-fact answer to "did the substrate see the references
// it should have?".
//
// Signal source: grounding_events.prompt_id, populated by the Stop-hook
// post-session processor (grounding-events-processor) from the
// transcript JSONL's promptId field. Online emits leave prompt_id
// empty, so this metric is meaningful only after a session ends.
// In-flight sessions are correctly excluded — they have no completed
// prompts to count.
//
// Query semantics:
//   - observed_prompts: distinct non-empty prompt_id values in the
//     window. Each represents one user prompt the substrate had any
//     grounding_events activity for (agent-initiated tool calls,
//     hook-emitted interception decisions, OR reference-resolution fires).
//   - with_parse_context: distinct prompt_ids that ALSO had at least
//     one row with query_source = 'reference_resolution' — the sole
//     query_source value the resolution path emits (refresolve/
//     grounding_emit.go), for BOTH the parse_context and the
//     resolve_references actions. NB the action NAME is parse_context,
//     but the query_source it stamps is reference_resolution; they are
//     not the same field. 'parse_context' is NOT a query_source value —
//     the grounding_events.query_source CHECK constraint does not admit
//     it, so filtering on it would match zero rows. (If the emitted set
//     is ever widened, add the new literal here alongside the writer +
//     CHECK change.)
//   - skipped_prompt_ids: up to 20 prompt_ids that observed activity
//     but no reference-resolution fire — the operator-debuggable cohort.
//
// Window default: 24 hours. ?hours= overrides; clamped to [1, 720]
// (one month) to keep the index scan bounded.
//
// The route is registered inside the router's Pool != nil gate, so the
// handler can assume s.Pool is non-nil — Pool-less boots return 404
// from the ServeMux rather than a custom 503.
func (s AppState) parseContextSkipRate(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			hours = parsed
			if hours > 720 {
				hours = 720
			}
		}
	}

	windowSpec := "-" + strconv.Itoa(hours) + " hours"

	var observed, withParse int64
	err := s.Pool.DB().QueryRowContext(r.Context(), `
		WITH window_rows AS (
		  SELECT prompt_id, query_source
		  FROM grounding_events
		  WHERE prompt_id IS NOT NULL AND prompt_id != ''
		    AND created_at > datetime('now', ?)
		),
		observed AS (
		  SELECT DISTINCT prompt_id FROM window_rows
		),
		parsed AS (
		  SELECT DISTINCT prompt_id FROM window_rows
		  WHERE query_source = 'reference_resolution'
		)
		SELECT
		  (SELECT count(*) FROM observed),
		  (SELECT count(*) FROM parsed)`,
		windowSpec,
	).Scan(&observed, &withParse)
	if err != nil {
		dbErr(w, err)
		return
	}

	resp := parseContextSkipRateResponse{
		WindowHours:      hours,
		ObservedPrompts:  observed,
		WithParseContext: withParse,
	}
	if observed > 0 {
		resp.SkipRate = 1.0 - float64(withParse)/float64(observed)
	}

	if observed > withParse {
		rows, err := s.Pool.DB().QueryContext(r.Context(), `
			SELECT DISTINCT prompt_id FROM grounding_events
			WHERE prompt_id IS NOT NULL AND prompt_id != ''
			  AND created_at > datetime('now', ?)
			  AND prompt_id NOT IN (
			    SELECT DISTINCT prompt_id FROM grounding_events
			    WHERE prompt_id IS NOT NULL AND prompt_id != ''
			      AND created_at > datetime('now', ?)
			      AND query_source = 'reference_resolution'
			  )
			ORDER BY prompt_id
			LIMIT 20`,
			windowSpec, windowSpec,
		)
		if err != nil {
			dbErr(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var pid string
			if err := rows.Scan(&pid); err != nil {
				dbErr(w, err)
				return
			}
			resp.SkippedPromptIDs = append(resp.SkippedPromptIDs, pid)
		}
		if err := rows.Err(); err != nil {
			dbErr(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
