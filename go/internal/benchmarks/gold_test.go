package benchmarks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadGoldScenarios_RoundTripsAllFields constructs a temp gold corpus
// + verifies the loader produces ClassifyScenarios with slug/text/rubric
// wired up + the correct gold-label shape.
func TestLoadGoldScenarios_RoundTripsAllFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.jsonl")
	content := strings.Join([]string{
		`{"slug": "alpha", "label": "low", "text": "first scenario"}`,
		`{"slug": "beta", "label": "unclassifiable", "text": "honesty case"}`,
		`{"slug": "gamma", "label": "a|b", "text": "multi-class"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadGoldScenarios("demo", dir)
	if err != nil {
		t.Fatalf("LoadGoldScenarios: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 scenarios, got %d", len(got))
	}

	if got[0].Slug != "alpha" || got[0].Text != "first scenario" {
		t.Errorf("alpha row: %+v", got[0])
	}
	if got[0].RubricSlug != "demo" {
		t.Errorf("rubric slug not propagated: %q", got[0].RubricSlug)
	}
	if got[0].Gold.Kind != GoldSingleClass || got[0].Gold.SingleLabel != "low" {
		t.Errorf("alpha gold: %+v", got[0].Gold)
	}

	if got[1].Gold.Kind != GoldUnclassifiable {
		t.Errorf("beta should be Unclassifiable, got %+v", got[1].Gold)
	}

	if got[2].Gold.Kind != GoldMultiClass {
		t.Errorf("gamma should be MultiClass, got %+v", got[2].Gold)
	}
	if len(got[2].Gold.MultiLabel) != 2 || got[2].Gold.MultiLabel[0] != "a" || got[2].Gold.MultiLabel[1] != "b" {
		t.Errorf("gamma labels: %v", got[2].Gold.MultiLabel)
	}
}

func TestLoadGoldScenarios_SkipsBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blank.jsonl")
	content := "\n" +
		`{"slug": "x", "label": "low", "text": "t"}` + "\n" +
		"\n" +
		`{"slug": "y", "label": "high", "text": "t2"}` + "\n" +
		"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadGoldScenarios("blank", dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 (blank lines skipped), got %d", len(got))
	}
}

func TestLoadGoldScenarios_ReturnsErrorOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadGoldScenarios("nope", dir)
	if err == nil {
		t.Error("missing file should error")
	}
}

func TestLoadGoldScenarios_ReturnsErrorOnMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGoldScenarios("bad", dir)
	if err == nil {
		t.Error("malformed JSON should error")
	}
}

func TestDefaultGoldDir_HonorsEnvOverride(t *testing.T) {
	t.Setenv(GoldCorpusEnv, "/custom/gold/dir")
	if got := DefaultGoldDir(); got != "/custom/gold/dir" {
		t.Errorf("env override: want /custom/gold/dir, got %q", got)
	}
}

func TestIsKnownE4Rubric(t *testing.T) {
	if !IsKnownE4Rubric("agentic-audit") {
		t.Errorf("agentic-audit should be known")
	}
	if IsKnownE4Rubric("not-a-rubric") {
		t.Errorf("not-a-rubric should NOT be known")
	}
}

// TestKnownE4Rubrics_HaveOnDiskGoldCorpora is a sanity check that the
// production gold-corpus directory ships a JSONL for every rubric in
// KnownE4Rubrics. Skipped when the directory isn't present (CI).
func TestKnownE4Rubrics_HaveOnDiskGoldCorpora(t *testing.T) {
	dir := DefaultGoldDir()
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("gold-corpus dir %s not present (CI / fresh checkout): %v", dir, err)
	}
	for _, r := range KnownE4Rubrics {
		path := filepath.Join(dir, r+".jsonl")
		if _, err := os.Stat(path); err != nil {
			t.Errorf("rubric %s: missing gold corpus at %s", r, path)
		}
	}
}
