package curation_test

import (
	"context"
	"errors"
	"testing"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
)

// stubBuilder is a SourceMaterialBuilder for registry tests.
type stubBuilder struct {
	origin string
	out    string
	err    error
}

func (s *stubBuilder) Origin() string { return s.origin }
func (s *stubBuilder) Build(_ context.Context, _ *db.Pool, _ curation.Candidate) (string, error) {
	return s.out, s.err
}

func TestBuilderRegistry_RegisterAndForOrigin(t *testing.T) {
	reg := curation.NewBuilderRegistry()
	reg.Register(&stubBuilder{origin: "task_handoff", out: "task material"})
	reg.Register(&stubBuilder{origin: "vault_note", out: "vault material"})

	b, err := reg.ForOrigin("task_handoff")
	if err != nil {
		t.Fatalf("ForOrigin: %v", err)
	}
	got, err := b.Build(context.Background(), nil, curation.Candidate{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got != "task material" {
		t.Errorf("Build: want %q, got %q", "task material", got)
	}
}

func TestBuilderRegistry_ForOriginUnknownReturnsTypedError(t *testing.T) {
	reg := curation.NewBuilderRegistry()
	reg.Register(&stubBuilder{origin: "task_handoff"})

	_, err := reg.ForOrigin("nonexistent_origin")
	if err == nil {
		t.Fatal("ForOrigin: want error on unknown, got nil")
	}
	if !errors.Is(err, curation.ErrUnknownOrigin) {
		t.Errorf("ForOrigin error: want errors.Is(err, ErrUnknownOrigin), got %v", err)
	}
}

func TestBuilderRegistry_RegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register: want panic on duplicate origin, got none")
		}
	}()
	reg := curation.NewBuilderRegistry()
	reg.Register(&stubBuilder{origin: "task_handoff"})
	reg.Register(&stubBuilder{origin: "task_handoff"}) // duplicate
}

func TestBuilderRegistry_OriginsEnumeratesAll(t *testing.T) {
	reg := curation.NewBuilderRegistry()
	reg.Register(&stubBuilder{origin: "a"})
	reg.Register(&stubBuilder{origin: "b"})
	reg.Register(&stubBuilder{origin: "c"})

	origins := reg.Origins()
	if len(origins) != 3 {
		t.Fatalf("Origins: want 3, got %d (%v)", len(origins), origins)
	}
	seen := map[string]bool{}
	for _, o := range origins {
		seen[o] = true
	}
	for _, want := range []string{"a", "b", "c"} {
		if !seen[want] {
			t.Errorf("Origins: missing %q", want)
		}
	}
}

// TestNewOriginExtensionRecipe pins the contract that adding a new
// origin requires only: (1) implement SourceMaterialBuilder, (2) call
// Register. If this test ever needs more steps to satisfy, the
// abstraction has leaked and the design doc's §8 claim is broken.
func TestNewOriginExtensionRecipe(t *testing.T) {
	reg := curation.NewBuilderRegistry()
	// Step 1: implement the interface (above, stubBuilder).
	// Step 2: register.
	reg.Register(&stubBuilder{origin: "hypothetical_future_origin", out: "ok"})
	// That's it. Lookup works, build works.
	b, err := reg.ForOrigin("hypothetical_future_origin")
	if err != nil {
		t.Fatalf("new-origin lookup: %v", err)
	}
	got, err := b.Build(context.Background(), nil, curation.Candidate{})
	if err != nil || got != "ok" {
		t.Errorf("new-origin build: got=%q err=%v", got, err)
	}
}
