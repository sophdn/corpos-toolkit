package rubric_test

import (
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/rubric"
)

func TestNewRegistry_LoadsValidRubric(t *testing.T) {
	r, err := rubric.NewRegistry("testdata/valid-dir")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	def, ok := r.Get("test-rubric")
	if !ok {
		t.Fatal("expected test-rubric to be registered")
	}
	if def.Name != "test-rubric" {
		t.Errorf("name: want test-rubric, got %q", def.Name)
	}
	if !def.IsDeployed {
		t.Error("expected is_deployed=true")
	}
	if len(def.OutputEnum) != 3 {
		t.Errorf("output_enum len: want 3, got %d", len(def.OutputEnum))
	}
	if len(def.Examples) != 2 {
		t.Errorf("examples len: want 2, got %d", len(def.Examples))
	}
}

func TestNewRegistry_MissingNameReturnsError(t *testing.T) {
	dir := t.TempDir()
	src, _ := os.ReadFile("testdata/missing-name.toml")
	_ = os.WriteFile(filepath.Join(dir, "missing-name.toml"), src, 0o600)

	_, err := rubric.NewRegistry(dir)
	if err == nil {
		t.Error("expected error for rubric missing name field, got nil")
	}
}

func TestRegistry_GetMiss(t *testing.T) {
	r, err := rubric.NewRegistry("testdata/valid-dir")
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, ok := r.Get("no-such-rubric")
	if ok {
		t.Error("expected Get miss for unknown rubric")
	}
}

func TestRegistry_ReloadPicksUpNewFile(t *testing.T) {
	dir := t.TempDir()
	src, _ := os.ReadFile("testdata/valid.toml")
	_ = os.WriteFile(filepath.Join(dir, "valid.toml"), src, 0o600)

	r, err := rubric.NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, ok := r.Get("test-rubric"); !ok {
		t.Fatal("test-rubric not found before reload")
	}

	// Write a new rubric file.
	newTOML := `name = "new-rubric"
description = "Added after initial load"
is_deployed = false
output_enum = ["a", "b"]
prompt_template = "classify"
`
	_ = os.WriteFile(filepath.Join(dir, "new-rubric.toml"), []byte(newTOML), 0o600)

	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := r.Get("new-rubric"); !ok {
		t.Error("new-rubric not found after reload")
	}
}

func TestNewRegistry_InvalidDirReturnsError(t *testing.T) {
	_, err := rubric.NewRegistry("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for nonexistent dir, got nil")
	}
}
