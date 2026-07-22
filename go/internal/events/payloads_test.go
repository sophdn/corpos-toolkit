package events_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"testing"

	"toolkit/internal/events"
)

// TestTaskCreatedPayload_TolerateStringAcceptanceCriteria covers the
// migration 056 Option-A backfill drift: synthetic TaskCreated events
// for pre-substrate tasks stamp acceptance_criteria as the raw CRUD
// column value (a JSON string), where the schema declares []string.
// `toolkit-server rebuild-projections` replays every TaskCreated event
// and previously crashed on the first such legacy event; the custom
// UnmarshalJSON wraps a string value as a single-element list so the
// fold's downstream join-on-"\n- " produces the same projection bytes
// the snapshot-seed left. Bug
// task-synthetic-event-backfill-stringtypes-acceptance-criteria-constraints.
func TestTaskCreatedPayload_TolerateStringAcceptanceCriteria(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "canonical array shape — current production handlers",
			payload: `{"chain_slug":"c","problem_statement":"p","acceptance_criteria":["a","b"]}`,
			want:    []string{"a", "b"},
		},
		{
			name: "legacy synthetic string — migration 056 Option-A backfill",
			payload: `{"chain_slug":"c","problem_statement":"p",` +
				`"acceptance_criteria":"line one\n- line two"}`,
			want: []string{"line one\n- line two"},
		},
		{
			name:    "empty string treated as nil",
			payload: `{"chain_slug":"c","problem_statement":"p","acceptance_criteria":""}`,
			want:    nil,
		},
		{
			name:    "missing key treated as nil",
			payload: `{"chain_slug":"c","problem_statement":"p"}`,
			want:    nil,
		},
		{
			name:    "explicit null treated as nil",
			payload: `{"chain_slug":"c","problem_statement":"p","acceptance_criteria":null}`,
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p events.TaskCreatedPayload
			if err := json.Unmarshal([]byte(tt.payload), &p); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(p.AcceptanceCriteria, tt.want) {
				t.Errorf("AcceptanceCriteria: got %#v want %#v", p.AcceptanceCriteria, tt.want)
			}
		})
	}
}

// TestRoadmapUpdatedPayload_SynthesizeItemsFromLegacyPositions covers
// the pre-T5-roadmap legacy event shape: payloads denormalized the per-
// position layout into top-level positions[] + ref_kind + ref_slug
// instead of the items[] array T5's additive bump (ca65006)
// introduced. `toolkit-server rebuild-projections` replays these
// events; the fold's insert/update branches required items[] and
// previously crashed with "lacks items[]". The custom UnmarshalJSON
// synthesizes items[] from the legacy fields so the fold's downstream
// path works unchanged. Twelve such legacy events live in the
// production events log as of 2026-05-21.
func TestRoadmapUpdatedPayload_SynthesizeItemsFromLegacyPositions(t *testing.T) {
	payload := `{"action_kind":"insert","positions":[15],` +
		`"ref_kind":"chain","ref_slug":"parse-context-lean-orienting"}`
	var p events.RoadmapUpdatedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.Items) != 1 {
		t.Fatalf("Items: got len %d want 1 (synthesized)", len(p.Items))
	}
	if p.Items[0].Position != 15 || p.Items[0].RefKind != "chain" ||
		p.Items[0].RefSlug != "parse-context-lean-orienting" {
		t.Errorf("synthesized item mismatch: %+v", p.Items[0])
	}
	// Canonical shape (items[] present) round-trips without synthesis.
	canonical := `{"action_kind":"set","items":[` +
		`{"position":1,"ref_kind":"chain","ref_slug":"c1"}]}`
	var q events.RoadmapUpdatedPayload
	if err := json.Unmarshal([]byte(canonical), &q); err != nil {
		t.Fatalf("unmarshal canonical: %v", err)
	}
	if len(q.Items) != 1 || q.Items[0].RefSlug != "c1" {
		t.Errorf("canonical items[] not preserved: %+v", q.Items)
	}
	// mark_reassessed legitimately has no positions+ref_kind+ref_slug;
	// must NOT synthesize anything.
	reassess := `{"action_kind":"mark_reassessed"}`
	var r events.RoadmapUpdatedPayload
	if err := json.Unmarshal([]byte(reassess), &r); err != nil {
		t.Fatalf("unmarshal mark_reassessed: %v", err)
	}
	if len(r.Items) != 0 {
		t.Errorf("mark_reassessed synthesized items: %+v", r.Items)
	}
}

// TestSchemaEvolution_NoInPlaceMinLengthTightening is the guard described in
// bug per-type-event-schemas-tightened-in-place-orphan-historical-events.
//
// Rationale: the "rename, don't mutate" convention (documented in
// go/internal/events/events.go SchemaVersion comment) prohibits adding
// stricter constraints (minLength, minimum, enum narrowing) to an existing
// per-type payload schema in-place. Doing so creates a ledger-consistency
// hazard: events that were valid at emit time become invalid under the
// current schema, breaking strict-replay and any consumer that assumes
// "every stored event satisfies its current type schema". The correct
// path is a type rename (e.g. ChainCreated → ChainCreatedV2).
//
// The guard detects the specific regression class that produced the
// ~4300 orphan events: a new "minLength" constraint added in-place to a
// non-versioned type. It compares the minLength count in each type's
// embedded schema against a frozen baseline. Any increase in a baseline
// type is a test failure with a remediation hint.
//
// HOW TO UPDATE THIS TEST:
//   - If you are adding a BRAND NEW type (no baseline entry): no action
//     needed. New types are not in the baseline and pass unconditionally.
//   - If you need to add a minLength constraint to an existing type: you
//     MUST rename the type (e.g. ChainCreated → ChainCreatedV2) and add a
//     new baseline entry for the renamed type. Do NOT increase the count for
//     the old name.
//   - If a constraint is REMOVED (count decreases): that is safe; update the
//     baseline entry to the new lower count with a comment.
func TestSchemaEvolution_NoInPlaceMinLengthTightening(t *testing.T) {
	// frozenMinLengthBaseline is the sealed count of "minLength" occurrences
	// per registered type as of the bug-resolution commit. This is the
	// maximum allowed count for each type. Types not in this map are new and
	// may carry any count (they have no historical events to orphan).
	//
	// Counts were derived from the embedded schemas/ directory at the time
	// bug per-type-event-schemas-tightened-in-place-orphan-historical-events
	// was resolved. The ChainCreated (3) and TaskCreated (2) counts are
	// already the tightened values that produced the ~4300 orphan events —
	// those constraints are grandfathered as the immutable baseline
	// (the events cannot be retroactively fixed; append-only ledger).
	frozenMinLengthBaseline := map[string]int{
		// ── types with minLength constraints (alphabetical) ──────────────────
		"BatchExecuted":             2,
		"BenchmarkBaselineUpdated":  3,
		"BenchmarkDiff":             2,
		"BenchmarkForged":           3,
		"BenchmarkRunCompleted":     1,
		"BenchmarkRunFailed":        2,
		"BenchmarkRunStarted":       8,
		"BugReported":               2,
		"ChainAndTasksForged":       2,
		"ChainCreated":              3, // grandfathered — orphaned ~4300 events
		"CurationCandidateRejected": 1,
		"MemoryWritten":             3,
		"MetricRecorded":            3,
		"MigrationForged":           2,
		"ReportCardForged":          2,
		"RetrospectiveForged":       2,
		"SuggestionReported":        2,
		"TaskAssignedToChain":       1,
		"TaskCreated":               2, // grandfathered — orphaned ~4300 events
		"TaskHandoff":               5,
		// ── types with zero minLength constraints are omitted; they are
		//    implicitly at 0 and any increase from 0 for a baseline type
		//    would not be caught. To catch zero→N transitions, add them
		//    explicitly below with count 0.
		"ActionDocsFrontendAuditCompleted":                     0,
		"ArcCloseAuthoringResolved":                            0,
		"ArcCloseFilingReviewed":                               0,
		"ArcCloseFilingReviewSubstrateAuditCompleted":          0,
		"ArcCloseFilingReviewSubstrateListenerWiringCompleted": 0,
		"ArchitectureAuditCompleted":                           0,
		"ArcReviewListenerFired":                               0,
		"BugEdited":                                            0,
		"BugReopened":                                          0,
		"BugResolved":                                          0,
		"BugStamped":                                           0,
		"BugTriaged":                                           0,
		"ChainClosed":                                          0,
		"ChainEdited":                                          0,
		"CommitLanded":                                         0,
		"ConventionAuditCompleted":                             0,
		"CurationCandidatePromoted":                            0,
		"EscalationContractAuditCompleted":                     0,
		"EscalationProposed":                                   0,
		"MemorySubstrateAuditCompleted":                        0,
		"MLCapabilityAuditCompleted":                           0,
		"ObservabilityAuditCompleted":                          0,
		"ParseContextDisciplineSurfaced":                       0,
		"ParseContextIntentResolved":                           0,
		"ParseContextKiwixFallbackFired":                       0,
		"ParseContextStdioDriftSurfaced":                       0,
		"ParseContextWorkStateSurfaced":                        0,
		"ReferenceResolutionAuditCompleted":                    0,
		"ReferenceResolutionFrontendAuditCompleted":            0,
		"ReferenceResolutionMigrationAuditCompleted":           0,
		"RoadmapUpdated":                                       0,
		"SubstrateFrontendAuditCompleted":                      0,
		"SuggestionEdited":                                     0,
		"SuggestionReopened":                                   0,
		"SuggestionResolved":                                   0,
		"SuggestionStamped":                                    0,
		"TaskCancelled":                                        0,
		"TaskCompleted":                                        0,
		"TaskEdited":                                           0,
		"TaskRetired":                                          0,
		"TaskStamped":                                          0,
		"TaskTransitioned":                                     0,
		"TelemetryAuditCompleted":                              0,
		"TelemetryFrontendAuditCompleted":                      0,
	}

	// versionSuffixRe matches type names ending in V followed by one or
	// more digits (e.g. ChainCreatedV2, BugResolvedV3). Versioned types are
	// exempt from the baseline check: they are the correct rename outcome.
	versionSuffixRe := regexp.MustCompile(`V\d+$`)

	minLengthToken := []byte(`"minLength"`)

	types := events.RegisteredTypes()
	if len(types) == 0 {
		t.Fatal("RegisteredTypes() returned empty — cannot run guard")
	}

	var failures []string
	for _, typ := range types {
		if versionSuffixRe.MatchString(typ) {
			// Versioned rename — not subject to the in-place tightening rule.
			continue
		}
		baseline, inBaseline := frozenMinLengthBaseline[typ]
		if !inBaseline {
			// Brand-new type: no historical events; any constraint count is fine.
			continue
		}

		raw, err := events.SchemaBytes(typ)
		if err != nil {
			t.Errorf("SchemaBytes(%q): %v", typ, err)
			continue
		}
		actual := bytes.Count(raw, minLengthToken)
		if actual > baseline {
			failures = append(failures, fmt.Sprintf(
				"  %s: baseline=%d actual=%d — schema gained %d new minLength constraint(s) in-place.\n"+
					"    FIX: rename the type (e.g. %sV2) instead of tightening in-place, or\n"+
					"    update frozenMinLengthBaseline if the type has NO historical events yet.",
				typ, baseline, actual, actual-baseline, typ,
			))
		}
	}

	if len(failures) > 0 {
		t.Errorf("schema-evolution violation: %d type(s) gained stricter minLength constraints in-place:\n%s\n"+
			"Per the 'rename, don't mutate' convention (events.go SchemaVersion comment),\n"+
			"tightening a per-type schema in-place orphans historical events that were\n"+
			"valid at emit time. Use a type rename (e.g. ChainCreated → ChainCreatedV2).\n"+
			"See bug: per-type-event-schemas-tightened-in-place-orphan-historical-events",
			len(failures), joinLines(failures))
	}
}

// joinLines joins a slice of strings with newlines. A small inline helper so
// the guard test can format multi-type failures as a block.
func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}
