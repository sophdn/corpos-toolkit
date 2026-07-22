package projections_test

import (
	"strings"
	"testing"

	"toolkit/internal/projections"
)

// TestAll_RespectsDependsOn pins the topological invariant: every
// projection that declares DependsOn appears AFTER each name in its
// dependency list. Independent projections are still alphabetical
// (tie-breaking rule), so this also asserts the legacy ordering for
// the non-dependent projections.
func TestAll_RespectsDependsOn(t *testing.T) {
	all := projections.All()
	position := map[string]int{}
	for i, p := range all {
		position[p.Name()] = i
	}

	expectedDeps := map[string][]string{
		"chain_status":  {"current_tasks"},
		"task_blockers": {"chain_status", "current_tasks"},
		"roadmap_view":  {"chain_status", "current_tasks"},
	}
	for name, deps := range expectedDeps {
		pos, ok := position[name]
		if !ok {
			t.Errorf("expected projection %q registered", name)
			continue
		}
		for _, dep := range deps {
			depPos, ok := position[dep]
			if !ok {
				t.Errorf("dependency %q for %q is not registered", dep, name)
				continue
			}
			if depPos >= pos {
				t.Errorf("%s (pos=%d) must follow its dependency %s (pos=%d) — actual order: %v",
					name, pos, dep, depPos, namesOf(all))
			}
		}
	}
}

// TestAll_IndependentProjectionsAlphabetical pins the tie-break
// rule: projections without DependsOn appear in alphabetical order
// among themselves, preserving the pre-dependency-injection ordering
// for everyone who never reads another projection's table.
func TestAll_IndependentProjectionsAlphabetical(t *testing.T) {
	dependents := map[string]bool{
		"chain_status":  true,
		"task_blockers": true,
		"roadmap_view":  true,
	}
	all := projections.All()
	prev := ""
	for _, p := range all {
		if dependents[p.Name()] {
			continue
		}
		if prev != "" && p.Name() < prev {
			t.Errorf("independent projection %q sorted before %q — alphabetical tie-break violated; order: %v",
				p.Name(), prev, namesOf(all))
		}
		prev = p.Name()
	}
}

// TestAll_IsDeterministic pins the no-thrash invariant: repeated
// All() calls return the same slice ordering. Topological sort with a
// stable alphabetical tie-break must not depend on map-iteration
// non-determinism.
func TestAll_IsDeterministic(t *testing.T) {
	first := namesOf(projections.All())
	for i := 0; i < 20; i++ {
		got := namesOf(projections.All())
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("non-deterministic All() on iteration %d:\n  first=%v\n  got  =%v", i, first, got)
		}
	}
}

func namesOf(ps []projections.Projection) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

// TestAll_ProductionDependenciesResolve sanity-checks that the
// production registry actually contains every projection named by a
// DependsOn declaration. Catches the failure mode where a projection
// gets renamed but its declared dep doesn't update — All() would
// panic at the first call, but this test pins the registration set
// without requiring a panic-recover.
func TestAll_ProductionDependenciesResolve(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range projections.All() {
		registered[p.Name()] = true
	}
	// Use the same name list as TestAll_RespectsDependsOn to keep the
	// expectations centralized.
	dependents := []struct {
		name string
		deps []string
	}{
		{"chain_status", []string{"current_tasks"}},
		{"task_blockers", []string{"chain_status", "current_tasks"}},
		{"roadmap_view", []string{"chain_status", "current_tasks"}},
	}
	for _, d := range dependents {
		if !registered[d.name] {
			t.Errorf("dependent %q not registered (expected to be production-active)", d.name)
		}
		for _, dep := range d.deps {
			if !registered[dep] {
				t.Errorf("%q declares DependsOn %q which is not registered", d.name, dep)
			}
		}
	}
}

// TestAll_NamesUnique asserts All() returns each projection exactly once —
// a double Register() (e.g. a copy-paste init()) would surface here. It
// replaces the former TestAll_CountUnchanged, which pinned a literal 12-name
// inventory duplicated in TestRegistry_AllProjections — a merge-conflict
// surface for parallel agents adding projections (chain
// worktree-multi-agent-orchestration-support T7). The de-registration canary
// that list provided now lives, derivation-based, in
// TestRegistry_AllProjections (registered TableName()s ↔ proj_* schema tables).
func TestAll_NamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range projections.All() {
		if seen[p.Name()] {
			t.Errorf("projections.All() returned duplicate name %q", p.Name())
		}
		seen[p.Name()] = true
	}
}
