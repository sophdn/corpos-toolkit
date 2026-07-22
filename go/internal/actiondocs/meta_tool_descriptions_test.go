package actiondocs

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// reference-resolution-migration T8: parity test between the hand-kept
// meta-tool descriptions in this package and the action-docs corpus at
// blueprints/action-docs/<surface>/. The action lists in each meta-tool
// description must match the .toml files in the corresponding corpus
// directory (excluding the `_general.toml` reserved chunk).
//
// Why this gate: bug task-block-action-doc-advertises-blocked_by-
// canonical-but-handler-only-accepts-blocker_slug (commit ea953e8)
// taught us that doc-vs-implementation drift silently produces wrong
// behavior. The meta-tool descriptions are the agent's index of what
// actions exist; an action present in the corpus but missing from the
// description is invisible to the agent until they look it up. An
// action listed in the description but missing from the corpus is a
// stale promise.
//
// Per the parity-tests-between-live-classifiers learning (2026-05-18),
// the right invariant is output-equivalence: the description's action
// list and the corpus's action set MUST be identical as sets. Test runs
// in scripts/precommit.sh via the standard `go test ./internal/actiondocs/`
// step; drift never lands.

// actionListPattern extracts the alphabetical action list from a meta-
// tool description. The description format is:
//
//	<purpose sentence>
//	<blank line>
//	Actions (alphabetical): act1, act2, ..., actN.
//	<blank line>
//	For per-action details ...
//
// The regex captures the comma-separated list between "Actions (alphabetical): "
// and the trailing period.
var actionListPattern = regexp.MustCompile(`(?m)^Actions \(alphabetical\): (.+?)\.$`)

func extractActionList(t *testing.T, description string, surface string) []string {
	t.Helper()
	match := actionListPattern.FindStringSubmatch(description)
	if match == nil {
		t.Fatalf("%s description missing 'Actions (alphabetical): ...' line", surface)
	}
	raw := match[1]
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func corpusActionList(t *testing.T, surface string) []string {
	t.Helper()
	root := actionDocsRoot(t)
	reg, err := Load(root)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	names := reg.Names(surface) // already sorted; excludes _general
	sort.Strings(names)
	return names
}

func TestMetaToolDescriptions_ActionListParity(t *testing.T) {
	cases := []struct {
		surface     string
		description string
	}{
		{"work", WorkDescription},
		{"measure", MeasureDescription},
		{"knowledge", KnowledgeDescription},
		{"admin", AdminDescription},
		{"ml", MLDescription},
		{"fs", FsDescription},
		{"sys", SysDescription},
	}
	for _, c := range cases {
		t.Run(c.surface, func(t *testing.T) {
			fromDescription := extractActionList(t, c.description, c.surface)
			fromCorpus := corpusActionList(t, c.surface)

			descSet := toSet(fromDescription)
			corpusSet := toSet(fromCorpus)

			// In description but missing from corpus — stale promise.
			var missingFromCorpus []string
			for action := range descSet {
				if !corpusSet[action] {
					missingFromCorpus = append(missingFromCorpus, action)
				}
			}
			sort.Strings(missingFromCorpus)
			if len(missingFromCorpus) > 0 {
				t.Errorf(
					"%s: description lists actions not in corpus (stale): %v",
					c.surface, missingFromCorpus,
				)
			}

			// In corpus but missing from description — invisible to the
			// agent until they look it up. This is the bug shape that
			// motivated the gate (resolve_references in knowledge T4 of
			// the substrate trilogy was missing from KnowledgeDescription
			// when this gate was first added).
			var missingFromDescription []string
			for action := range corpusSet {
				if !descSet[action] {
					missingFromDescription = append(missingFromDescription, action)
				}
			}
			sort.Strings(missingFromDescription)
			if len(missingFromDescription) > 0 {
				t.Errorf(
					"%s: corpus has actions not listed in description (invisible to agent): %v",
					c.surface, missingFromDescription,
				)
			}
		})
	}
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// Sympathy test: confirm the gate catches a synthetic drift. Construct a
// description-with-missing-action and a description-with-stale-action;
// assert the comparison logic flags both.
func TestMetaToolDescriptions_GateCatchesSyntheticDrift(t *testing.T) {
	corpus := corpusActionList(t, "work")
	if len(corpus) == 0 {
		t.Skip("no corpus actions; sympathy test inapplicable")
	}

	// Drop the first action from the description; expect detection.
	missingOne := corpus[1:]
	descMissing := buildSyntheticDescription("Synthetic-purpose.", missingOne)
	fromDescMissing := extractActionList(t, descMissing, "work")
	fromCorpus := corpusActionList(t, "work")
	var detectedMissing []string
	descSet := toSet(fromDescMissing)
	for _, action := range fromCorpus {
		if !descSet[action] {
			detectedMissing = append(detectedMissing, action)
		}
	}
	if len(detectedMissing) == 0 || detectedMissing[0] != corpus[0] {
		t.Errorf("gate failed to detect missing %q (got: %v)", corpus[0], detectedMissing)
	}

	// Add a synthetic action not in the corpus; expect detection.
	stalePadded := append([]string{}, corpus...)
	stalePadded = append(stalePadded, "_synthetic_stale_action_zzz")
	sort.Strings(stalePadded)
	descStale := buildSyntheticDescription("Synthetic-purpose.", stalePadded)
	fromDescStale := extractActionList(t, descStale, "work")
	corpusSet := toSet(fromCorpus)
	var detectedStale []string
	for _, action := range fromDescStale {
		if !corpusSet[action] {
			detectedStale = append(detectedStale, action)
		}
	}
	if len(detectedStale) == 0 || detectedStale[0] != "_synthetic_stale_action_zzz" {
		t.Errorf("gate failed to detect stale action (got: %v)", detectedStale)
	}
}

func buildSyntheticDescription(purpose string, actions []string) string {
	return purpose + "\n\nActions (alphabetical): " + strings.Join(actions, ", ") + "."
}
