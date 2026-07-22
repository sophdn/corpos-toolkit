package refresolve_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/refresolve"
)

// intentFixture mirrors the JSON shape in testdata/directive_intent_fixtures.json.
type intentFixture struct {
	Fixtures []struct {
		Shape   string `json:"shape"`
		Prompts []struct {
			Text string `json:"text"`
			Note string `json:"note"`
		} `json:"prompts"`
	} `json:"fixtures"`
}

func loadIntentFixture(t *testing.T) intentFixture {
	t.Helper()
	path := filepath.Join("testdata", "directive_intent_fixtures.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fix intentFixture
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return fix
}

// TestDetectIntent_FixtureRecall pins the T4 §13.5 latency-budget
// constraint: pattern recall must be ≥80% on the §13.7 fixture
// corpus for the pattern-first path to remain viable (sub-100μs)
// without the Qwen-rubric backstop.
//
// Reports per-shape breakdown so a recall regression is locatable.
// Failing this test means either the fixture grew faster than the
// pattern set or a pattern needs tightening — both are calls to
// open a follow-on chain task per T4's design rather than expand
// the closed vocabulary in place.
func TestDetectIntent_FixtureRecall(t *testing.T) {
	fix := loadIntentFixture(t)
	type shapeStats struct {
		total int
		hits  int
		miss  []string
	}
	stats := map[string]*shapeStats{}
	totalPrompts := 0
	totalHits := 0
	for _, sh := range fix.Fixtures {
		ss := &shapeStats{}
		stats[sh.Shape] = ss
		for _, p := range sh.Prompts {
			ss.total++
			totalPrompts++
			got := refresolve.DetectIntent(p.Text)
			if string(got.Shape) == sh.Shape {
				ss.hits++
				totalHits++
				continue
			}
			ss.miss = append(ss.miss, "got="+string(got.Shape)+" prompt="+p.Text)
		}
	}
	for _, sh := range fix.Fixtures {
		ss := stats[sh.Shape]
		t.Logf("shape=%s recall=%d/%d", sh.Shape, ss.hits, ss.total)
		for _, m := range ss.miss {
			t.Logf("  miss: %s", m)
		}
	}
	recallPct := float64(totalHits) / float64(totalPrompts) * 100
	t.Logf("overall recall=%d/%d (%.1f%%)", totalHits, totalPrompts, recallPct)
	if recallPct < 80.0 {
		t.Errorf("overall recall %.1f%% < 80%% threshold (T4 §13.5)", recallPct)
	}
}

// TestDetectIntent_NamedT4Prompts pins the four real-session prompts
// T4's acceptance criteria called out by name. These are
// load-bearing — T5 MUST classify them correctly or revisit the
// pattern set (T4 §13.7).
func TestDetectIntent_NamedT4Prompts(t *testing.T) {
	cases := []struct {
		text string
		want refresolve.IntentShape
	}{
		{"please sanity check go-toolkit-dry-extraction-audit-followup", refresolve.IntentVerify},
		{"please implement that fix after filing it", refresolve.IntentImplement},
		{"Any cleanup to do?", refresolve.IntentAudit},
		{"I'd like the banner to work properly", refresolve.IntentFix},
	}
	for _, tc := range cases {
		got := refresolve.DetectIntent(tc.text)
		if got.Shape != tc.want {
			t.Errorf("DetectIntent(%q).Shape = %q, want %q (DetectedVia=%q)",
				tc.text, got.Shape, tc.want, got.DetectedVia)
		}
	}
}

// TestDetectIntent_RefactorShapeClassifiesAsAudit pins the audit-mapping
// chosen by chain refactor-intent-discipline-surfacing over a new
// IntentRefactor shape: refactor-intent directives carrying NO literal
// trigger keyword classify as audit (refactor-intent ⊆ audit). These are
// the keyword-free gap the keyword skill_trigger path misses.
func TestDetectIntent_RefactorShapeClassifiesAsAudit(t *testing.T) {
	cases := []string{
		"this function does too much, break it apart",
		"make this easier to follow",
		"this method is doing way too much",
		"this code is too tangled — untangle it",
	}
	for _, msg := range cases {
		got := refresolve.DetectIntent(msg)
		if got.Shape != refresolve.IntentAudit {
			t.Errorf("DetectIntent(%q).Shape = %q, want audit (DetectedVia=%q)", msg, got.Shape, got.DetectedVia)
		}
	}
}

// TestDetectIntent_NoMatchReturnsNone confirms the closed-vocabulary
// invariant from T4 §13.2: a prompt that fits no shape resolves to
// IntentNone (not a speculative invented shape).
func TestDetectIntent_NoMatchReturnsNone(t *testing.T) {
	cases := []string{
		"thanks",
		"ok cool",
		"that's a good point about the cache invalidation",
		"",
	}
	for _, msg := range cases {
		got := refresolve.DetectIntent(msg)
		if got.Shape != refresolve.IntentNone {
			t.Errorf("DetectIntent(%q).Shape = %q, want IntentNone", msg, got.Shape)
		}
	}
}

// TestDetectIntent_EmptyMessageReturnsNone is a degenerate-input
// guard: empty / whitespace-only messages classify as IntentNone
// without panicking.
func TestDetectIntent_EmptyMessageReturnsNone(t *testing.T) {
	if got := refresolve.DetectIntent(""); got.Shape != refresolve.IntentNone {
		t.Errorf("empty: Shape = %q, want IntentNone", got.Shape)
	}
	if got := refresolve.DetectIntent("   \n\t  "); got.Shape != refresolve.IntentNone {
		t.Errorf("whitespace: Shape = %q, want IntentNone", got.Shape)
	}
}
