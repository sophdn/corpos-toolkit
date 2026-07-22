package observehttp

import (
	"fmt"
	"net/http"
	"sort"
	"testing"

	"toolkit/internal/testutil"
)

// Regression tests for bug `context-pulls-first-candidate-source-type-
// empty-due-to-source-ref-format-mismatch`.
//
// Pre-fix behavior: /context-pulls JOINed grounding_events.source_refs
// (entries authored as `<type>:<rest>`) against knowledge_pointers.
// source_ref (rows keyed as `<project>::<slug>`). The two key formats
// never matched, so every row's first_candidate.source_type came out
// empty AND the ?source_type=... filter silently dropped every row.
// 100% miss rate observed on the live DB across every prefix (skill,
// chain, schema, project, memory, path, bug).
//
// Post-fix: source_type derives from the prefix of source_refs[0]
// directly; no knowledge_pointers JOIN. These tests seed grounding_
// events with NO knowledge_pointers rows at all — if the dedupe were
// re-introduced, the assertions below would fail because the JOIN's
// LEFT side would have nothing to bind to.

// TestContextPullsList_SourceTypeFromPrefix_NoPointerNeeded covers the
// canonical bug shape: every row's first_candidate.source_type must
// populate from the source_refs[0] prefix even when knowledge_pointers
// is empty for that source_ref.
func TestContextPullsList_SourceTypeFromPrefix_NoPointerNeeded(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// No knowledge_pointers seeded. Pre-fix this guaranteed empty
	// source_type on every row; post-fix the source_type is derived
	// from the source_refs prefix.
	prefixes := []string{"chain", "skill", "schema", "vault", "memory", "path"}
	for i, pfx := range prefixes {
		seedRefResRow(t, pool, int64(100+i), "p1", func(ge *geSeed, _ *rreSeed) {
			ge.CallID = fmt.Sprintf("c%d", 100+i)
			ge.SourceRefs = fmt.Sprintf(`[%q]`, pfx+":fixture-slug")
		})
	}

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	if code := getJSON(t, srv, "/context-pulls", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Items) != len(prefixes) {
		t.Fatalf("got %d items, want %d", len(resp.Items), len(prefixes))
	}
	gotByType := map[string]bool{}
	for _, it := range resp.Items {
		if it.FirstCandidate == nil {
			t.Errorf("ge=%d: first_candidate nil (expected populated from source_refs prefix)", it.GroundingEventID)
			continue
		}
		if it.FirstCandidate.SourceType == "" {
			t.Errorf("ge=%d: source_type EMPTY (regression — bug %s)",
				it.GroundingEventID,
				"context-pulls-first-candidate-source-type-empty-due-to-source-ref-format-mismatch")
		}
		gotByType[it.FirstCandidate.SourceType] = true
	}
	for _, pfx := range prefixes {
		if !gotByType[pfx] {
			t.Errorf("expected at least one row with source_type=%q, got none", pfx)
		}
	}
}

// TestContextPullsList_SourceTypeFilter_FromPrefix covers the filter
// path. Pre-fix the ?source_type= filter JOINed knowledge_pointers and
// so silently dropped every row regardless of input. Post-fix the
// filter is a LIKE-prefix match on the source_refs JSON array.
func TestContextPullsList_SourceTypeFilter_FromPrefix(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Three rows, three different source_type prefixes. No
	// knowledge_pointers rows seeded — the filter must work without
	// them.
	seedRefResRow(t, pool, 101, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-skill"
		ge.SourceRefs = `["skill:body/path/foo.md"]`
	})
	seedRefResRow(t, pool, 102, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-chain"
		ge.SourceRefs = `["chain:rust-retirement"]`
	})
	seedRefResRow(t, pool, 103, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-schema"
		ge.SourceRefs = `["schema:blueprints/forge-schemas/bug.toml"]`
	})

	srv := newAuditServer(t, pool)
	cases := []struct {
		filter   string
		wantID   int64
		wantType string
	}{
		{"skill", 101, "skill"},
		{"chain", 102, "chain"},
		{"schema", 103, "schema"},
	}
	for _, c := range cases {
		var resp contextPullListResponse
		getJSON(t, srv, "/context-pulls?source_type="+c.filter, &resp)
		if len(resp.Items) != 1 {
			t.Errorf("filter=%q: got %d items, want 1: %+v",
				c.filter, len(resp.Items), idsOf(resp.Items))
			continue
		}
		got := resp.Items[0]
		if got.GroundingEventID != c.wantID {
			t.Errorf("filter=%q: id=%d, want %d", c.filter, got.GroundingEventID, c.wantID)
		}
		if got.FirstCandidate == nil || got.FirstCandidate.SourceType != c.wantType {
			t.Errorf("filter=%q: source_type=%+v, want %q",
				c.filter, got.FirstCandidate, c.wantType)
		}
	}
}

// TestContextPullsList_SourceTypeFilter_MultiValue covers the OR-combo
// case where the caller supplies multiple source_type values. Both
// prefixes must match (any-of semantics).
func TestContextPullsList_SourceTypeFilter_MultiValue(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	seedRefResRow(t, pool, 201, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-skill"
		ge.SourceRefs = `["skill:s1"]`
	})
	seedRefResRow(t, pool, 202, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-vault"
		ge.SourceRefs = `["vault:v1"]`
	})
	seedRefResRow(t, pool, 203, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-chain"
		ge.SourceRefs = `["chain:c1"]`
	})

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls?source_type=skill&source_type=chain", &resp)
	gotIDs := idsOf(resp.Items)
	sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
	wantIDs := []int64{201, 203}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("got %d items, want 2: ids=%v", len(gotIDs), gotIDs)
	}
	for i, w := range wantIDs {
		if gotIDs[i] != w {
			t.Errorf("ids[%d]=%d, want %d (full=%v)", i, gotIDs[i], w, gotIDs)
		}
	}
}

// TestContextPullsAvailableSourceTypes_FromGroundingEvents covers the
// filter-dropdown population. Pre-fix the values came from a DISTINCT
// SELECT on knowledge_pointers, which had a different (and disjoint)
// set of source_types from what appeared in grounding_events.
// Post-fix the dropdown reads from the live source_refs prefixes the
// resolvers actually emit.
func TestContextPullsAvailableSourceTypes_FromGroundingEvents(t *testing.T) {
	pool := testutil.NewTestDB(t)
	seedProject(t, pool, "p1")
	// Seed grounding_events with three distinct prefixes; seed
	// knowledge_pointers with a fourth prefix that should NOT appear in
	// the dropdown.
	seedRefResRow(t, pool, 301, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-1"
		ge.SourceRefs = `["skill:body/foo.md"]`
	})
	seedRefResRow(t, pool, 302, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-2"
		ge.SourceRefs = `["schema:blueprints/bar.toml"]`
	})
	seedRefResRow(t, pool, 303, "p1", func(ge *geSeed, _ *rreSeed) {
		ge.CallID = "c-3"
		ge.SourceRefs = `["chain:c1"]`
	})
	// A pointer with source_type='kiwix_reference' that does NOT appear
	// in any grounding_event. Must NOT appear in the dropdown.
	seedKnowledgePointer(t, pool, "p1", "kiwix_reference", "p1::ghost", "ghost-q", 0)

	srv := newAuditServer(t, pool)
	var resp contextPullListResponse
	getJSON(t, srv, "/context-pulls", &resp)
	got := map[string]bool{}
	for _, v := range resp.AvailableSourceTypes {
		got[v] = true
	}
	for _, want := range []string{"skill", "schema", "chain"} {
		if !got[want] {
			t.Errorf("available_source_types missing %q (got %v)",
				want, resp.AvailableSourceTypes)
		}
	}
	if got["kiwix_reference"] {
		t.Errorf("available_source_types should NOT include orphan pointer source_type %q (got %v) — pre-fix bug: dropdown read from knowledge_pointers instead of live grounding_events",
			"kiwix_reference", resp.AvailableSourceTypes)
	}
}

func idsOf(items []contextPullRow) []int64 {
	out := make([]int64, len(items))
	for i, it := range items {
		out[i] = it.GroundingEventID
	}
	return out
}
