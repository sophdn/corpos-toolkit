package benchmarks

import (
	"testing"

	"toolkit/internal/qwenretrieve"
)

// Mirrors Rust test extract_scenario_can_be_constructed_from_owned_strings
// (benchmarks/src/scenario_types.rs:166). Same behavior: build a scenario
// from owned strings, assert slug + field count.
func TestExtractScenario_ConstructFromOwnedStrings(t *testing.T) {
	s := ExtractScenario{
		Slug: "test-bug",
		TaskInput: ExtractTaskInput{
			ProblemStatement: "x",
			Source:           "y",
			Project:          "z",
		},
		Context: ExtractContext{
			SchemaName: "bug",
			SchemaFields: []SchemaField{{
				Name:        "title",
				Description: "one-line desc",
				Required:    true,
				Kind:        "string",
			}},
		},
		GoldOutput: ExtractGoldOutput{
			Fields: map[string]ExpectedFieldValue{
				"title": {Kind: FieldPresent},
			},
		},
	}
	if s.Slug != "test-bug" {
		t.Errorf("Slug: want test-bug, got %q", s.Slug)
	}
	if len(s.Context.SchemaFields) != 1 {
		t.Errorf("SchemaFields len: want 1, got %d", len(s.Context.SchemaFields))
	}
	if s.GoldOutput.Fields["title"].Kind != FieldPresent {
		t.Errorf("title field kind: want FieldPresent, got %v", s.GoldOutput.Fields["title"].Kind)
	}
}

// Mirrors Rust test classify_scenario_honesty_case_uses_unclassifiable_gold
// (benchmarks/src/scenario_types.rs:198). Behavior: an honesty-case scenario
// stores GoldUnclassifiable so the grader treats "unclassifiable" as
// the honest response.
func TestClassifyScenario_HonestyCaseUsesUnclassifiableGold(t *testing.T) {
	s := ClassifyScenario{
		Slug:       "test-honesty",
		Text:       "off-rubric prose",
		RubricSlug: "test-rubric",
		Gold:       ClassifyGold{Kind: GoldUnclassifiable},
	}
	if s.Gold.Kind != GoldUnclassifiable {
		t.Errorf("Gold.Kind: want GoldUnclassifiable, got %v", s.Gold.Kind)
	}
}

// Mirrors Rust test retrieve_scenario_honesty_case_ignores_gold_path
// (benchmarks/src/scenario_types.rs:216). Behavior: honesty-case retrieve
// scenarios mark HonestyCase=true so the grader ignores GoldPath.
func TestRetrieveScenario_HonestyCaseIgnoresGoldPath(t *testing.T) {
	s := RetrieveScenario{
		Slug: "test-no-match",
		TaskInput: qwenretrieve.RetrieveTaskInput{
			Query: "kubernetes",
			TopK:  3,
		},
		Context: qwenretrieve.RetrieveContext{
			CorpusShape: qwenretrieve.CorpusShapeVault,
		},
		GoldPath:    "ignored-when-honesty-case",
		HonestyCase: true,
	}
	if !s.HonestyCase {
		t.Errorf("HonestyCase: want true, got false")
	}
}

// Extends Rust coverage: ExpectedFieldValue variants round-trip the
// discriminator + per-variant fields without aliasing. Caught when
// extracting the JaccardAtLeast variant: forgot Threshold; would have
// silently graded with 0.0.
func TestExpectedFieldValue_VariantsCarryFields(t *testing.T) {
	cases := []struct {
		name string
		v    ExpectedFieldValue
		want ExpectedFieldValueKind
	}{
		{"exact", ExpectedFieldValue{Kind: FieldExact, Value: "foo"}, FieldExact},
		{"present", ExpectedFieldValue{Kind: FieldPresent}, FieldPresent},
		{"one-of", ExpectedFieldValue{Kind: FieldOneOf, OneOf: []string{"a", "b"}}, FieldOneOf},
		{"contains", ExpectedFieldValue{Kind: FieldContains, Value: "abc"}, FieldContains},
		{"jaccard", ExpectedFieldValue{Kind: FieldJaccardAtLeast, GoldSet: []string{"x"}, Threshold: 0.5}, FieldJaccardAtLeast},
	}
	for _, tc := range cases {
		if tc.v.Kind != tc.want {
			t.Errorf("%s: Kind want %v, got %v", tc.name, tc.want, tc.v.Kind)
		}
	}
	jaccard := ExpectedFieldValue{Kind: FieldJaccardAtLeast, GoldSet: []string{"x", "y"}, Threshold: 0.5}
	if jaccard.Threshold != 0.5 {
		t.Errorf("Jaccard.Threshold: want 0.5, got %v", jaccard.Threshold)
	}
	if len(jaccard.GoldSet) != 2 {
		t.Errorf("Jaccard.GoldSet len: want 2, got %d", len(jaccard.GoldSet))
	}
}
