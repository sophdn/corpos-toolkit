package arcparams_test

// Characterization net for the arc-close param structs in their relocated home
// (chain establish-action-doc-contract-on-work, C1 — extracted verbatim from
// internal/arcreview into this leaf package). The structs carry no behavior of
// their own; their entire contract is the JSON wire-binding — which is what the
// action-handlers unmarshal into AND what the work action-doc registry derives
// each documented param's type from. This net pins that binding across
// acceptance / boundary / rejection input classes so a future tag or field-kind
// edit that would silently break the wire contract (or the downstream
// type-derivation) fails loudly instead.
//
// Per refactoring-discipline step 7: characterize the newly-exposed unit
// boundary across its own input-classes. (The struct-move itself was parity-
// proven by the compiler + the existing internal/arcreview suite; this is the
// densification of the new seam.)

import (
	"encoding/json"
	"reflect"
	"testing"

	"toolkit/internal/arcreview/arcparams"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestReviewArcForFilingParams_Binding(t *testing.T) {
	// Acceptance: every documented key binds to its field, including the
	// []string trigger list and the int turn-counter.
	const full = `{
		"session_id": "sess-1",
		"fired_at": "2026-05-24T12:00:00Z",
		"triggers": ["user_shape_done", "counter_user_turns_5"],
		"user_turns_since_review": 5,
		"transcript_path": "/home/u/.claude/projects/x/session.jsonl"
	}`
	var p arcparams.ReviewArcForFilingParams
	if err := json.Unmarshal([]byte(full), &p); err != nil {
		t.Fatalf("acceptance: unexpected error: %v", err)
	}
	want := arcparams.ReviewArcForFilingParams{
		SessionID:            "sess-1",
		FiredAt:              "2026-05-24T12:00:00Z",
		Triggers:             []string{"user_shape_done", "counter_user_turns_5"},
		UserTurnsSinceReview: 5,
		TranscriptPath:       "/home/u/.claude/projects/x/session.jsonl",
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("acceptance bind mismatch:\n got %+v\nwant %+v", p, want)
	}

	// Boundary: empty object → all zero values (the handler's "session_id is
	// required" guard keys off the zero string, so this binding is load-bearing).
	var empty arcparams.ReviewArcForFilingParams
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatalf("empty: unexpected error: %v", err)
	}
	if !reflect.DeepEqual(empty, arcparams.ReviewArcForFilingParams{}) {
		t.Errorf("empty object should bind to zero value, got %+v", empty)
	}

	// Boundary: unknown keys are ignored (plain json.Unmarshal contract — the
	// handlers do NOT DisallowUnknownFields, so extra detector keys don't error).
	var extra arcparams.ReviewArcForFilingParams
	if err := json.Unmarshal([]byte(`{"session_id":"s","unrecognized":true}`), &extra); err != nil {
		t.Fatalf("unknown-key: unexpected error: %v", err)
	}
	if extra.SessionID != "s" {
		t.Errorf("unknown-key: session_id should still bind, got %q", extra.SessionID)
	}
}

func TestReviewArcForFilingParams_RejectsTypeMismatch(t *testing.T) {
	// Rejection: the int turn-counter and the []string trigger list are
	// type-strict — a wrong JSON type errors (handlers return status=skipped
	// "parse params"). Pins that the binding is not silently coercive.
	cases := map[string]string{
		"counter as string":  `{"user_turns_since_review":"five"}`,
		"triggers as scalar": `{"triggers":"user_shape_done"}`,
	}
	for name, payload := range cases {
		var p arcparams.ReviewArcForFilingParams
		if err := json.Unmarshal([]byte(payload), &p); err == nil {
			t.Errorf("%s: expected unmarshal error, got none (bound %+v)", name, p)
		}
	}
}

func TestEmitCommitLandedParams_OptionalPointers(t *testing.T) {
	// The optional fields are pointers precisely so absent (nil) is
	// distinguishable from present-but-empty. Pin all three cases.

	// Acceptance: every key present → required string set, optionals non-nil.
	const full = `{
		"commit_sha": "abc1234",
		"branch": "main",
		"files_changed_count": 3,
		"author": "sophdn <s@example.com>",
		"subject": "fix: thing"
	}`
	var p arcparams.EmitCommitLandedParams
	if err := json.Unmarshal([]byte(full), &p); err != nil {
		t.Fatalf("acceptance: unexpected error: %v", err)
	}
	want := arcparams.EmitCommitLandedParams{
		CommitSHA:         "abc1234",
		Branch:            strPtr("main"),
		FilesChangedCount: intPtr(3),
		Author:            strPtr("sophdn <s@example.com>"),
		Subject:           strPtr("fix: thing"),
	}
	if !reflect.DeepEqual(p, want) {
		t.Errorf("acceptance bind mismatch:\n got %+v\nwant %+v", p, want)
	}

	// Boundary: optionals OMITTED → nil pointers; required field still binds.
	var minimal arcparams.EmitCommitLandedParams
	if err := json.Unmarshal([]byte(`{"commit_sha":"abc1234"}`), &minimal); err != nil {
		t.Fatalf("minimal: unexpected error: %v", err)
	}
	if minimal.CommitSHA != "abc1234" {
		t.Errorf("minimal: commit_sha should bind, got %q", minimal.CommitSHA)
	}
	if minimal.Branch != nil || minimal.FilesChangedCount != nil || minimal.Author != nil || minimal.Subject != nil {
		t.Errorf("minimal: omitted optionals should be nil, got %+v", minimal)
	}

	// Boundary: explicit JSON null → nil pointer (same as absent).
	var nulled arcparams.EmitCommitLandedParams
	if err := json.Unmarshal([]byte(`{"commit_sha":"x","branch":null}`), &nulled); err != nil {
		t.Fatalf("null: unexpected error: %v", err)
	}
	if nulled.Branch != nil {
		t.Errorf("explicit null branch should bind to nil, got %v", *nulled.Branch)
	}
}

func TestArcReviewAuditParams_Binding(t *testing.T) {
	// Acceptance.
	var p arcparams.ArcReviewAuditParams
	if err := json.Unmarshal([]byte(`{"since":"2026-05-13T00:00:00Z","correction_window_hours":24}`), &p); err != nil {
		t.Fatalf("acceptance: unexpected error: %v", err)
	}
	if p.Since != "2026-05-13T00:00:00Z" || p.CorrectionWindowHours != 24 {
		t.Errorf("acceptance bind mismatch: got %+v", p)
	}

	// Boundary: empty object → zero values (the handler applies its 7-day /
	// 24h defaults off these zeros, so the zero binding is load-bearing).
	var empty arcparams.ArcReviewAuditParams
	if err := json.Unmarshal([]byte(`{}`), &empty); err != nil {
		t.Fatalf("empty: unexpected error: %v", err)
	}
	if empty != (arcparams.ArcReviewAuditParams{}) {
		t.Errorf("empty object should bind to zero value, got %+v", empty)
	}

	// Rejection: window as string is type-strict.
	var bad arcparams.ArcReviewAuditParams
	if err := json.Unmarshal([]byte(`{"correction_window_hours":"24"}`), &bad); err == nil {
		t.Errorf("expected type error for string window, got none")
	}
}

func TestPendingDecisionsClaimParams_Binding(t *testing.T) {
	// Acceptance.
	var p arcparams.PendingDecisionsClaimParams
	if err := json.Unmarshal([]byte(`{"session_id":"sess-9","limit":10}`), &p); err != nil {
		t.Fatalf("acceptance: unexpected error: %v", err)
	}
	if p.SessionID != "sess-9" || p.Limit != 10 {
		t.Errorf("acceptance bind mismatch: got %+v", p)
	}

	// Boundary: limit omitted → 0 (the handler's "default below applies when
	// omitted or zero" path keys off this).
	var noLimit arcparams.PendingDecisionsClaimParams
	if err := json.Unmarshal([]byte(`{"session_id":"sess-9"}`), &noLimit); err != nil {
		t.Fatalf("no-limit: unexpected error: %v", err)
	}
	if noLimit.SessionID != "sess-9" || noLimit.Limit != 0 {
		t.Errorf("no-limit: expected limit 0, got %+v", noLimit)
	}
}
