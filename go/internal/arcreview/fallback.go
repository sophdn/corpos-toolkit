package arcreview

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/arcreview/arcparams"
	"toolkit/internal/db"
	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// fallback.go — T5 of chain arc-close-decision-authoring-split. The
// unreviewed-fallback capture: a staged body-heavy decision that the
// in-session agent never authored (session end / explicit skip) is forged
// from Qwen's retained draft body, flagged `unreviewed` + `qwen-authored`,
// so capture is never lost and the thin note is findable for later
// enrichment. See docs/ARC_CLOSE_DECISION_AUTHORING_SPLIT.md
// §Unreviewed-fallback semantics.
//
// No-worse-than-today invariant: a fallback forge produces exactly the
// note today's auto-execute would have produced (Qwen's body) — only now
// marked unreviewed and findable. The split can therefore never regress
// capture: worst case == today, best case == an agent-authored note.

// authoring_state values on pending_decisions (see migration 081).
const (
	authoringStateStaged   = "staged"
	authoringStateAuthored = "authored"
	authoringStateFallback = "fallback_forged"
)

// fallbackGraceEnvVar overrides the grace window a staged row must age
// past before the sweep will fall it back. A non-zero grace gives the
// seated agent time to author before the substrate captures the draft.
const fallbackGraceEnvVar = "TOOLKIT_ARCCLOSE_FALLBACK_GRACE"

// fallbackDefaultGrace is the default age a staged row must exceed before
// the sweep forges its fallback. Sized so a within-session arc that the
// agent is actively authoring isn't reaped out from under them, while a
// genuinely abandoned staging (the disengaged-seat case) is captured on
// the next sweep. Operators tune via TOOLKIT_ARCCLOSE_FALLBACK_GRACE
// (Go duration syntax).
const fallbackDefaultGrace = 15 * time.Minute

// fallbackAuthoredMatchJaccard is the title-token similarity threshold
// above which a knowledge_pointers entry counts as "the agent authored
// this staged decision" — suppressing the fallback. Mirrors the F3
// same-session default; same-title authoring tends to be near-verbatim on
// the seed even when the body is fully rewritten.
const fallbackAuthoredMatchJaccard = 0.40

func fallbackGrace() time.Duration {
	raw := strings.TrimSpace(os.Getenv(fallbackGraceEnvVar))
	if raw == "" {
		return fallbackDefaultGrace
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return fallbackDefaultGrace
	}
	return d
}

// stagedRow is one pending_decisions row still in the 'staged' state.
type stagedRow struct {
	ID            int64
	EventID       string
	DecisionsJSON string
	CreatedAt     string
}

// SweepResult summarizes one fallback sweep for telemetry (T7) and the
// caller's log line.
type SweepResult struct {
	RowsScanned    int
	FallbackForged int // staged decisions forged as unreviewed drafts
	AuthoredSkips  int // staged decisions the agent already authored
	RowsMarked     int // pending_decisions rows transitioned out of 'staged'
}

// SweepUnauthoredStaged forges the unreviewed fallback for staged
// decisions in sessionID that the agent never authored, once the staging
// row has aged past the grace window. It is the session-end / explicit-skip
// capture point: when called, any still-'staged' decision older than the
// grace is treated as abandoned by the seat and its Qwen draft is forged
// (flagged unreviewed) so capture is not lost.
//
// Per-row outcome:
//   - every staged decision the agent already authored (a matching
//     knowledge_pointers / MemoryWritten artifact exists since staging) is
//     skipped; the row transitions to 'authored'.
//   - otherwise the draft is forged unreviewed and the row transitions to
//     'fallback_forged'.
//
// Fail-safe: a nil ForgeFn (forge not wired) returns early without touching
// state — a staged decision stays staged rather than being lost or the
// sweep crashing. Per-row forge errors log and leave that row 'staged' for
// a later retry.
func SweepUnauthoredStaged(ctx context.Context, deps Deps, project, sessionID string) (SweepResult, error) {
	var res SweepResult
	if deps.Pool == nil {
		return res, fmt.Errorf("arcreview fallback: pool is nil")
	}
	if sessionID == "" {
		return res, nil
	}
	if deps.ForgeFn == nil {
		obs.Logger(ctx).Warn("arcreview fallback: ForgeFn not wired; skipping unreviewed-fallback sweep (staged decisions retained)",
			"session_id", sessionID)
		return res, nil
	}

	// Match SQLite's datetime('now') storage format (space-separated, no
	// 'Z') that pending_decisions.created_at defaults to — an RFC3339 'T'
	// cutoff would mis-order lexically against it ('T' > ' ').
	cutoff := time.Now().Add(-fallbackGrace()).UTC().Format("2006-01-02 15:04:05")
	rows, err := loadStagedRows(ctx, deps.Pool, sessionID, cutoff)
	if err != nil {
		return res, fmt.Errorf("load staged rows: %w", err)
	}
	res.RowsScanned = len(rows)

	for _, row := range rows {
		sweepStagedRow(ctx, deps, project, row, &res)
	}

	if res.FallbackForged > 0 || res.AuthoredSkips > 0 {
		obs.Logger(ctx).Info("arcreview fallback: sweep complete",
			"session_id", sessionID,
			"rows_scanned", res.RowsScanned,
			"fallback_forged", res.FallbackForged,
			"authored_skips", res.AuthoredSkips,
			"rows_marked", res.RowsMarked)
		// T7 telemetry: emit one ArcCloseAuthoringResolved per sweep that
		// resolved staged decisions. The author-vs-fallback rate
		// (authored / (authored + fallback)) is the seat-strength
		// instrument. Emit failure is non-fatal — the capture already
		// landed; the event is the corpus row, not the load-bearing output.
		emitAuthoringResolved(ctx, deps.Pool, project, sessionID, res)
	}
	return res, nil
}

// emitAuthoringResolved writes one ArcCloseAuthoringResolved event for a
// sweep that resolved staged decisions. authored_count = AuthoredSkips
// (agent authored, fallback suppressed); fallback_forged_count =
// FallbackForged (Qwen draft forged unreviewed).
func emitAuthoringResolved(ctx context.Context, pool *db.Pool, project, sessionID string, res SweepResult) {
	if pool == nil {
		return
	}
	payload := events.ArcCloseAuthoringResolvedPayload{
		SessionID:           sessionID,
		AuthoredCount:       res.AuthoredSkips,
		FallbackForgedCount: res.FallbackForged,
		RowsMarked:          res.RowsMarked,
	}
	var entity events.EntityRef
	if project == "" {
		entity = events.NewCrossCuttingEntityRef("arc_review_session", sessionID)
	} else {
		entity = events.NewEntityRef("arc_review_session", sessionID, project)
	}
	err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, e := events.Emit(ctx, tx, events.EmitArgs{Entity: entity, Payload: payload})
		return e
	})
	if err != nil {
		obs.Logger(ctx).Warn("arcreview fallback: ArcCloseAuthoringResolved emit failed (non-fatal)",
			"session_id", sessionID, "err", err.Error())
	}
}

// sweepStagedRow processes one staged row: forges the unreviewed fallback
// for each unauthored staged decision and transitions the row's
// authoring_state. Accumulates counts into res. A per-decision forge
// failure leaves the whole row 'staged' (no state transition) so the next
// sweep retries it rather than half-capturing the row.
func sweepStagedRow(ctx context.Context, deps Deps, project string, row stagedRow, res *SweepResult) {
	var decisions []FilingDecision
	if err := json.Unmarshal([]byte(row.DecisionsJSON), &decisions); err != nil {
		obs.Logger(ctx).Warn("arcreview fallback: row decisions_json unparseable; leaving staged",
			"row_id", row.ID, "err", err.Error())
		return
	}
	forgedAny := false
	for i := range decisions {
		d := &decisions[i]
		if !d.StagedForAuthoring {
			continue
		}
		authored, aerr := agentAuthoredMatching(ctx, deps.Pool, project, d)
		if aerr != nil {
			obs.Logger(ctx).Warn("arcreview fallback: authored-check failed; treating as unauthored",
				"row_id", row.ID, "err", aerr.Error())
		}
		if authored {
			res.AuthoredSkips++
			continue
		}
		if ferr := forgeUnreviewedFallback(ctx, deps, project, d); ferr != nil {
			obs.Logger(ctx).Warn("arcreview fallback: forge failed; leaving row staged for retry",
				"row_id", row.ID, "action", string(d.Action), "err", ferr.Error())
			return // retry the whole row next sweep; do not transition.
		}
		res.FallbackForged++
		forgedAny = true
	}
	// Row resolved: at least one forged → fallback_forged; else (every
	// staged decision was already authored) → authored.
	newState := authoringStateAuthored
	if forgedAny {
		newState = authoringStateFallback
	}
	if merr := markRowAuthoringState(ctx, deps.Pool, row.ID, newState); merr != nil {
		obs.Logger(ctx).Warn("arcreview fallback: state update failed",
			"row_id", row.ID, "target_state", newState, "err", merr.Error())
		return
	}
	res.RowsMarked++
}

// HandleSweepUnauthoredStaged is the MCP action wrapper around
// SweepUnauthoredStaged. It is the explicit "defined trigger" surface
// (session end / agent skip): a hook or the agent calls it to capture any
// staged decision the seat never authored. Non-error on an empty/quiet
// sweep (the steady state).
func HandleSweepUnauthoredStaged(ctx context.Context, deps Deps, project string, params json.RawMessage) (SweepResult, error) {
	var p arcparams.SweepUnauthoredStagedParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return SweepResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.SessionID == "" {
		return SweepResult{}, fmt.Errorf("session_id is required")
	}
	return SweepUnauthoredStaged(ctx, deps, project, p.SessionID)
}

// loadStagedRows returns pending_decisions rows still in 'staged' for the
// session that were created at or before cutoff (aged past the grace).
func loadStagedRows(ctx context.Context, pool *db.Pool, sessionID, cutoff string) ([]stagedRow, error) {
	rs, err := pool.DB().QueryContext(ctx, `
		SELECT id, event_id, decisions_json, created_at
		FROM pending_decisions
		WHERE target_session_id = ?
		  AND authoring_state = ?
		  AND created_at <= ?
		ORDER BY created_at ASC, id ASC`,
		sessionID, authoringStateStaged, cutoff)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []stagedRow
	for rs.Next() {
		var r stagedRow
		if err := rs.Scan(&r.ID, &r.EventID, &r.DecisionsJSON, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// markRowAuthoringState transitions a pending_decisions row's
// authoring_state. Only rows currently 'staged' are updated, so a
// concurrent sweep can't double-transition a row.
func markRowAuthoringState(ctx context.Context, pool *db.Pool, rowID int64, state string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE pending_decisions
			SET authoring_state = ?
			WHERE id = ? AND authoring_state = ?`,
			state, rowID, authoringStateStaged)
		return err
	})
}

// agentAuthoredMatching reports whether the in-session agent already
// authored an artifact matching this staged decision since it was staged —
// in which case the fallback is suppressed (the agent did the work).
//
//   - forge_vault_note: a knowledge_pointers row of source_type 'vault'
//     whose question (the note title) is Jaccard-near the staged seed title.
//   - memory_write: a MemoryWritten event whose name matches.
//
// No time bound is needed: F2's pre-filing dedupe already demotes (never
// stages) a decision that duplicates a PRE-EXISTING artifact, so any title
// match at sweep time is a note the agent authored THIS session.
//
// Best-effort: a query error is returned to the caller, which treats it as
// "unauthored" (forge the fallback) — capture-not-lost dominates a possible
// duplicate, which is itself findable + dedup-cleanable.
func agentAuthoredMatching(ctx context.Context, pool *db.Pool, project string, d *FilingDecision) (bool, error) {
	seedTitle := decisionTitle(d)
	if strings.TrimSpace(seedTitle) == "" {
		return false, nil
	}
	seedTokens := titleTokens(seedTitle)
	if len(seedTokens) == 0 {
		return false, nil
	}

	switch d.Action {
	case ActionForgeVaultNote:
		rs, err := pool.DB().QueryContext(ctx, `
			SELECT question FROM knowledge_pointers
			WHERE project_id = ? AND source_type = 'vault'`, project)
		if err != nil {
			return false, err
		}
		defer rs.Close()
		for rs.Next() {
			var q string
			if err := rs.Scan(&q); err != nil {
				return false, err
			}
			if similarity(seedTokens, titleTokens(q)) >= fallbackAuthoredMatchJaccard {
				return true, nil
			}
		}
		return false, rs.Err()
	case ActionMemoryWrite:
		// MemoryWritten events carry the entry name in the payload; a
		// matching name means the agent wrote it this session.
		rs, err := pool.DB().QueryContext(ctx, `
			SELECT json_extract(payload, '$.name')
			FROM events
			WHERE type = 'MemoryWritten'`)
		if err != nil {
			return false, err
		}
		defer rs.Close()
		for rs.Next() {
			var name sql.NullString
			if err := rs.Scan(&name); err != nil {
				return false, err
			}
			if name.Valid && similarity(seedTokens, titleTokens(name.String)) >= fallbackAuthoredMatchJaccard {
				return true, nil
			}
		}
		return false, rs.Err()
	}
	return false, nil
}

// unreviewedBodySentinel prefixes a fallback-forged body so the note is
// self-describing: a human (or cleanup sweep) opening it immediately sees
// it is a Qwen draft the seat never reviewed.
const unreviewedBodySentinel = "> ⚠️ **Qwen-authored draft, unreviewed.** The arc-close review decided this " +
	"was worth filing, but the in-session agent did not author it before the session ended. " +
	"Enrich with full context or delete. (arc-close decision/authoring-split fallback.)\n\n"

// forgeUnreviewedFallback forges one staged decision's retained Qwen draft
// via the injected ForgeFn, flagged `unreviewed` + `qwen-authored` (a
// queryable tag) with the body sentinel prepended. ForgeFn owns its tx.
func forgeUnreviewedFallback(ctx context.Context, deps Deps, project string, d *FilingDecision) error {
	params, err := unreviewedForgeParams(d)
	if err != nil {
		return err
	}
	return deps.ForgeFn(ctx, project, params)
}

// The fallback forge params are typed per schema (NOT map[string]any) to
// honor the concentrated-`any` lint boundary (forbidigo restricts bare any
// to internal/db + internal/dispatch). Each mirrors the forge action's
// raw-params shape — schema_name + slug + the schema's typed fields — so the
// injected ForgeFn (forge.HandleForge) decodes it unchanged.
type forgeFieldsVaultNote struct {
	NoteKind string `json:"note_kind"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Tags     string `json:"tags"`
}

type vaultNoteForgeParams struct {
	SchemaName string               `json:"schema_name"`
	Slug       string               `json:"slug"`
	Fields     forgeFieldsVaultNote `json:"fields"`
}

type forgeFieldsMemory struct {
	MemoryKind  string `json:"memory_kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Source      string `json:"source"`
}

type memoryForgeParams struct {
	SchemaName string            `json:"schema_name"`
	Slug       string            `json:"slug"`
	Fields     forgeFieldsMemory `json:"fields"`
}

// unreviewedForgeParams builds the forge params for a staged decision's
// unreviewed fallback. Qwen's draft body (retained in the decision payload
// since T4) is prefixed with the sentinel; the unreviewed/qwen-authored
// tags are appended so the note is queryable.
func unreviewedForgeParams(d *FilingDecision) (json.RawMessage, error) {
	switch d.Action {
	case ActionForgeVaultNote:
		var p ForgeVaultNotePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode vault-note payload: %w", err)
		}
		return json.Marshal(vaultNoteForgeParams{
			SchemaName: "vault-note",
			Slug:       slugify(p.Title),
			Fields: forgeFieldsVaultNote{
				NoteKind: p.NoteKind,
				Title:    p.Title,
				Body:     unreviewedBodySentinel + p.Body,
				Tags:     appendTags(p.Tags, "unreviewed", "qwen-authored"),
			},
		})
	case ActionMemoryWrite:
		var p MemoryWritePayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode memory payload: %w", err)
		}
		return json.Marshal(memoryForgeParams{
			SchemaName: "memory",
			Slug:       slugify(p.Name),
			Fields: forgeFieldsMemory{
				MemoryKind:  p.MemoryKind,
				Name:        p.Name,
				Description: p.Description,
				Body:        unreviewedBodySentinel + p.Body,
				Source:      "arc-close-fallback-unreviewed",
			},
		})
	}
	return nil, fmt.Errorf("unreviewed fallback: unsupported action %q", d.Action)
}

// appendTags joins existing comma-delimited tags with extra tags, deduping
// trivially and dropping empties.
func appendTags(existing string, extra ...string) string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range strings.Split(existing, ",") {
		add(t)
	}
	for _, t := range extra {
		add(t)
	}
	return strings.Join(out, ",")
}

// slugify lowercases and kebab-cases a title into a forge-safe slug,
// capped at 80 chars (matching the hook's forge_row cap).
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 80 {
		slug = strings.Trim(slug[:80], "-")
	}
	if slug == "" {
		slug = "arc-review-fallback-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return slug
}
