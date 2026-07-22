package events

import (
	"errors"
	"testing"
)

func TestCheckProvenanceText(t *testing.T) {
	tests := []struct {
		name string
		text string
		deny bool
	}{
		// ── DENY: an agent scope/deferral decision laundered as the user's, uncited ──
		{"chose to defer", "the user chose to defer the full port", true},
		{"the actual landmine", "SUPERSEDED DUPLICATE — closed per the user's 2026-06-26 scope decision to defer the full port", true},
		{"user-approved minimal scope", "user-approved minimal scope for the sessions surface", true},
		{"you decided to cut", "you decided to cut the importer down to the happy path", true},
		{"agreed to drop", "the user agreed to drop the graph edges", true},
		{"picked minimal", "the user picked the minimal sessions option", true},

		// ── ALLOW: the same attribution, but CITED ──
		{"cited with you-said", "the user chose to defer the full port. you said: keep it minimal for now", false},
		{"cited with a quote", `the user chose minimal sessions ("minimal real now")`, false},

		// ── ALLOW: requests, non-reduction decisions, agent-attributed, benign ──
		{"asked for minimal is a request not a decision", "the user asked for a minimal version", false},
		{"decided a direction, not a reduction", "the user decided to use Postgres", false},
		{"agent-attributed", "I scoped this to minimal; the full port is deferred to task 9", false},
		{"plain request mention", "implemented the user's wiki page request", false},
		{"empty", "", false},
		{"benign field text", "list + detail over name/played_at; no graph link", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkProvenanceText("payload.field", tc.text)
			if got := err != nil; got != tc.deny {
				t.Fatalf("text=%q: deny=%v (err=%v), want deny=%v", tc.text, got, err, tc.deny)
			}
			if err != nil {
				var inv *ErrInvalidInput
				if !errors.As(err, &inv) {
					t.Fatalf("want *ErrInvalidInput, got %T", err)
				}
			}
		})
	}
}

func TestCheckPayloadProvenance(t *testing.T) {
	mustDeny := func(name string, payload string) {
		t.Run("deny/"+name, func(t *testing.T) {
			if err := checkPayloadProvenance([]byte(payload)); err == nil {
				t.Fatalf("expected provenance rejection for %s", payload)
			}
		})
	}
	mustAllow := func(name string, payload []byte) {
		t.Run("allow/"+name, func(t *testing.T) {
			if err := checkPayloadProvenance(payload); err != nil {
				t.Fatalf("expected pass, got: %v", err)
			}
		})
	}

	mustDeny("top-level constraints field", `{"title":"x","constraints":"the user chose to defer the full port","n":1}`)
	mustDeny("nested array string", `{"meta":{"notes":["fine","you decided to skip the touches"]}}`)

	mustAllow("cited", []byte(`{"constraints":"the user chose to defer — you said: do the minimal one"}`))
	mustAllow("benign + non-string fields", []byte(`{"title":"Wiki page","count":3,"flags":[true,false],"body":"searchable list of entities"}`))
	mustAllow("empty payload", nil)
	mustAllow("empty object", []byte(`{}`))
	mustAllow("malformed is skipped (schema validator's job)", []byte(`not json`))
}
