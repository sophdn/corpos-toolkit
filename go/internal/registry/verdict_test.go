package registry_test

import (
	"context"
	"testing"
	"time"

	"toolkit/internal/registry"
)

// scriptedFetcher returns a pre-scripted sequence of states, one per call,
// repeating the last entry once exhausted.
type scriptedFetcher struct {
	states []string
	calls  int
}

func (s *scriptedFetcher) FetchStatus(_ context.Context, _ string) (string, string, error) {
	i := s.calls
	if i >= len(s.states) {
		i = len(s.states) - 1
	}
	s.calls++
	return s.states[i], "desc:" + s.states[i], nil
}

func TestPollVerdict_ResolvesToTerminalState(t *testing.T) {
	// pending, pending, then success → Blessed.
	f := &scriptedFetcher{states: []string{"pending", "pending", "success"}}
	v, err := registry.PollVerdict(context.Background(), f, "deadbeef", 10, time.Millisecond)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v.State != "success" || !v.Blessed() {
		t.Fatalf("expected blessed success, got %+v", v)
	}
	if f.calls != 3 {
		t.Fatalf("expected 3 polls (pending,pending,success), got %d", f.calls)
	}
}

func TestPollVerdict_Failure(t *testing.T) {
	f := &scriptedFetcher{states: []string{"failure"}}
	v, err := registry.PollVerdict(context.Background(), f, "deadbeef", 10, time.Millisecond)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v.State != "failure" || v.Blessed() {
		t.Fatalf("expected unblessed failure, got %+v", v)
	}
}

func TestPollVerdict_PendingPollOutIsNotAnError(t *testing.T) {
	// Never resolves within the attempt budget → returns pending, no error.
	f := &scriptedFetcher{states: []string{"pending"}}
	v, err := registry.PollVerdict(context.Background(), f, "deadbeef", 3, time.Millisecond)
	if err != nil {
		t.Fatalf("poll-out should not be an error: %v", err)
	}
	if v.State != "pending" {
		t.Fatalf("expected pending after poll-out, got %+v", v)
	}
}
