package arcreview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"toolkit/internal/db"
)

// F3 of chain arc-close-filing-review-dedupe-and-noise-reduction:
// same-session dedupe window. Within a single session_id, suppress
// arc-close decisions whose payload (title + problem_statement /
// body) is semantically equivalent to a prior decision in the same
// session's recent history. Retention default: 1 hour.
//
// Implementation: queries pending_decisions for prior fires in this
// session_id within the retention window. No new side-table —
// pending_decisions already stores per-fire decisions with their
// payloads; the F3 dedupe just reuses it.
//
// Distinct from F2 (similarity vs PROJECT-WIDE existing artifacts).
// F3 catches the "Qwen proposed the same payload across two arc-
// close fires in the SAME session" pattern. F2 catches the "Qwen
// proposed something that already exists in bug_list / suggestion_list".
// Both compose with F4's content-shape validation.

// sessionDedupeRetentionEnvVar is the env-var name for the retention
// horizon override. Useful for live tuning + tests.
const sessionDedupeRetentionEnvVar = "TOOLKIT_ARCCLOSE_DEDUPE_SESSION_RETENTION"

// sessionDedupeDefaultRetention is the default lookback window:
// 1 hour from the current fire's wall-clock time. Long enough to
// catch repeat-proposals across multiple back-to-back arc fires in
// a focused session; short enough that the SQL scan stays cheap.
const sessionDedupeDefaultRetention = 1 * time.Hour

// sessionDedupeRetention reads the env var (Go duration syntax:
// "30m", "2h", etc.) or returns the default. Unparseable values
// fall back to the default rather than panicking.
func sessionDedupeRetention() time.Duration {
	raw := strings.TrimSpace(os.Getenv(sessionDedupeRetentionEnvVar))
	if raw == "" {
		return sessionDedupeDefaultRetention
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return sessionDedupeDefaultRetention
	}
	return d
}

// sessionDedupeJaccardEnvVar overrides the same-session similarity
// threshold. Separate from the F2 threshold so the two filters can
// be tuned independently.
const sessionDedupeJaccardEnvVar = "TOOLKIT_ARCCLOSE_DEDUPE_SESSION_JACCARD_THRESHOLD"

// sessionDedupeDefaultJaccard is the default threshold. Higher than
// F2's 0.30 because same-session repeats tend to be near-verbatim
// (Qwen's two arc-close fires within one session see very similar
// snapshot content), so a stricter threshold doesn't lose recall
// while keeping false-positive rate low.
const sessionDedupeDefaultJaccard = 0.40

func sessionDedupeJaccardThreshold() float64 {
	raw := strings.TrimSpace(os.Getenv(sessionDedupeJaccardEnvVar))
	if raw == "" {
		return sessionDedupeDefaultJaccard
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 || v > 1 {
		return sessionDedupeDefaultJaccard
	}
	return v
}

// SameSessionMatch records a same-session dedupe hit. EventID names
// the prior ArcCloseFilingReviewed event that proposed the
// equivalent payload; Similarity is the Jaccard score on title +
// problem_statement / body tokens.
type SameSessionMatch struct {
	EventID    string  `json:"event_id"`
	Similarity float64 `json:"similarity"`
}

// PriorSessionDecision is a flattened view of one prior decision
// drawn from a pending_decisions row. Carries the source event_id
// + the decision's action + the parsed payload-derived signature
// (title + first-N body/problem tokens).
type PriorSessionDecision struct {
	EventID   string
	Action    ActionKind
	Signature map[string]struct{}
}

// LoadPriorSessionDecisions reads pending_decisions for the named
// session_id within the retention window and returns the parsed
// decisions. Fails open: any per-row parse failure logs and skips
// the row; the rest still flow through.
func LoadPriorSessionDecisions(ctx context.Context, pool *db.Pool, sessionID string, since time.Time) ([]PriorSessionDecision, error) {
	if pool == nil {
		return nil, fmt.Errorf("arcreview session-dedupe: pool is nil")
	}
	if sessionID == "" {
		return nil, nil
	}
	rows, err := pool.DB().QueryContext(ctx, `
		SELECT event_id, decisions_json
		FROM pending_decisions
		WHERE target_session_id = ?
		  AND created_at >= ?
		ORDER BY created_at ASC`,
		sessionID, since.UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PriorSessionDecision{}
	for rows.Next() {
		var eventID, decisionsJSON string
		if err := rows.Scan(&eventID, &decisionsJSON); err != nil {
			return nil, err
		}
		var ds []FilingDecision
		if err := json.Unmarshal([]byte(decisionsJSON), &ds); err != nil {
			// Skip malformed rows — corpus archaeology shouldn't
			// block the active dedupe pass.
			continue
		}
		for _, d := range ds {
			if d.Action == ActionNothingToFile {
				continue
			}
			sig := PayloadSignature(&d)
			if len(sig) == 0 {
				continue
			}
			out = append(out, PriorSessionDecision{
				EventID:   eventID,
				Action:    d.Action,
				Signature: sig,
			})
		}
	}
	return out, rows.Err()
}

// PayloadSignature returns the lowercased token set for a decision's
// title + first ~100 tokens of problem_statement / body. Used by
// F3 to compare prior-fire payloads against the current fire's
// proposals; exported so tests can construct PriorSessionDecision
// fixtures with the same tokenisation the production path uses.
func PayloadSignature(d *FilingDecision) map[string]struct{} {
	title := decisionTitle(d)
	body := decisionBodyOrProblem(d)
	combined := title + " " + body
	return titleTokens(combined)
}

// decisionBodyOrProblem extracts the long-form text from each
// action's payload: body for vault_note + memory_write,
// problem_statement for bug + suggestion. Returns empty for
// skill_update / nothing_to_file (their identifier shape is the
// slug or kind, not long-form text).
func decisionBodyOrProblem(d *FilingDecision) string {
	if len(d.Payload) == 0 {
		return ""
	}
	switch d.Action {
	case ActionForgeBug:
		var p ForgeBugPayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.ProblemStatement
		}
	case ActionForgeSuggestion:
		var p ForgeSuggestionPayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.ProblemStatement
		}
	case ActionForgeVaultNote:
		var p ForgeVaultNotePayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Body
		}
	case ActionMemoryWrite:
		var p MemoryWritePayload
		if err := json.Unmarshal(d.Payload, &p); err == nil {
			return p.Body
		}
	}
	return ""
}

// ApplySameSessionDedupe annotates each decision in result.Decisions
// with SameSessionDedupedAgainst when the payload signature exceeds
// the session-dedupe threshold against any prior decision of the
// same action kind. Mutates result.Decisions in place.
//
// Threshold via TOOLKIT_ARCCLOSE_DEDUPE_SESSION_JACCARD_THRESHOLD;
// default 0.40 (stricter than F2 because same-session repeats tend
// to be near-verbatim).
func ApplySameSessionDedupe(result *ArcReviewResult, priors []PriorSessionDecision) {
	if result == nil || len(priors) == 0 {
		return
	}
	threshold := sessionDedupeJaccardThreshold()
	for i := range result.Decisions {
		d := &result.Decisions[i]
		if d.Action == ActionNothingToFile {
			continue
		}
		sig := PayloadSignature(d)
		if len(sig) == 0 {
			continue
		}
		best := SameSessionMatch{}
		for _, prior := range priors {
			if prior.Action != d.Action {
				continue
			}
			sim := similarity(sig, prior.Signature)
			if sim > best.Similarity {
				best = SameSessionMatch{EventID: prior.EventID, Similarity: roundTo(sim, 3)}
			}
		}
		if best.Similarity >= threshold {
			d.SameSessionDedupedAgainst = &best
		}
	}
}
