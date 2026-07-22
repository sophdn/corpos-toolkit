package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/auditemit"
)

// TestCommittedSpecsValidate is the spec-rot guard: every spec shipped under
// specs/ must load and schema-validate against the live per-type event
// schema. If a schema changes incompatibly (or a spec is hand-edited into an
// invalid shape), this fails — the spec files are data the generic command
// depends on, so they are gated like code.
func TestCommittedSpecsValidate(t *testing.T) {
	specs, err := filepath.Glob("specs/*.json")
	if err != nil {
		t.Fatalf("glob specs: %v", err)
	}
	if len(specs) == 0 {
		t.Fatal("no specs found under specs/ — expected the migrated audit specs")
	}
	ctx := context.Background()
	for _, f := range specs {
		t.Run(filepath.Base(f), func(t *testing.T) {
			spec, err := auditemit.Load(f)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := auditemit.CheckSchema(ctx, spec); err != nil {
				t.Errorf("schema check: %v", err)
			}
		})
	}
}

func TestResolveSpecs(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"b.json", "a.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A directory resolves to its *.json files, sorted, excluding non-json.
	got, err := resolveSpecs(dir)
	if err != nil {
		t.Fatalf("resolveSpecs(dir): %v", err)
	}
	want := []string{filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("dir resolve = %v, want %v", got, want)
	}

	// A single file resolves to itself.
	single, err := resolveSpecs(filepath.Join(dir, "a.json"))
	if err != nil {
		t.Fatalf("resolveSpecs(file): %v", err)
	}
	if len(single) != 1 || single[0] != filepath.Join(dir, "a.json") {
		t.Errorf("file resolve = %v", single)
	}

	// A missing path errors.
	if _, err := resolveSpecs(filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error for missing path")
	}
}
