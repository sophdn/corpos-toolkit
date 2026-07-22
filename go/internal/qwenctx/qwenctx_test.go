package qwenctx_test

import (
	"context"
	"testing"

	"toolkit/internal/qwenctx"
)

func TestTaskID_DefaultsToUnattributed(t *testing.T) {
	if got := qwenctx.TaskID(context.Background()); got != qwenctx.Unattributed {
		t.Errorf("unstamped ctx: got %q, want %q", got, qwenctx.Unattributed)
	}
}

func TestWithTaskID_RoundTrip(t *testing.T) {
	ctx := qwenctx.WithTaskID(context.Background(), "vault-rerank-retrieve")
	if got := qwenctx.TaskID(ctx); got != "vault-rerank-retrieve" {
		t.Errorf("stamped ctx: got %q, want %q", got, "vault-rerank-retrieve")
	}
}

func TestWithTaskID_EmptyStringIgnored(t *testing.T) {
	// Empty stamps should not overwrite the upstream stamp — a downstream
	// handler that doesn't know its own task_id must not erase the parent's.
	parent := qwenctx.WithTaskID(context.Background(), "classify-pre-commit-failure")
	child := qwenctx.WithTaskID(parent, "")
	if got := qwenctx.TaskID(child); got != "classify-pre-commit-failure" {
		t.Errorf("empty stamp overwrote parent: got %q", got)
	}
}
