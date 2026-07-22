package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"toolkit/internal/dispatch"
)

// rejectEnvelope mimics the in-band rejection shape work handlers use:
// a non-empty Error string (with err == nil), optionally a Kind classifier.
type rejectEnvelope struct {
	Error string
	Kind  string
}

// ptrErrEnvelope + ptrRejectResult mimic the OTHER rejection shape (e.g.
// TaskReadResult): the failure rides on a pointer Err field, not an Error
// string — a non-nil pointer is a rejection.
type ptrErrEnvelope struct{ Error string }
type ptrRejectResult struct{ Err *ptrErrEnvelope }

// TestDispatch_CallObserver_CapturesOutcomes pins the thin telemetry spine
// (chain quiet-and-instrument-operator-surface T1): the observer fires once
// per dispatch with the right error_class across the outcome space — success
// (map + success-envelope), in-band rejection with/without a Kind, a handler
// Go error, and an unknown action. The in-band-rejection cases are the
// load-bearing ones: they return err == nil and would look like successes
// without the result-envelope classification.
func TestDispatch_CallObserver_CapturesOutcomes(t *testing.T) {
	type captured struct {
		action, project, errClass string
		latency                   time.Duration
	}
	var got []captured
	dispatch.SetCallObserver(func(_ context.Context, _ /*surface*/, action, project string, latency time.Duration, errClass string) {
		got = append(got, captured{action, project, errClass, latency})
	})
	t.Cleanup(func() { dispatch.SetCallObserver(nil) })

	ok := func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return map[string]string{"ok": "1"}, nil
	}
	table := dispatch.Table{
		"ok_map":      ok,
		"ok_envelope": func(_ context.Context, _ string, _ json.RawMessage) (any, error) { return rejectEnvelope{}, nil },
		"reject_kind": func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
			return rejectEnvelope{Error: "missing field x", Kind: "missing_required"}, nil
		},
		"reject_nokind": func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
			return rejectEnvelope{Error: "plain failure"}, nil
		},
		"boom": func(_ context.Context, _ string, _ json.RawMessage) (any, error) { return nil, errors.New("kaboom") },
		"reject_ptr": func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
			return ptrRejectResult{Err: &ptrErrEnvelope{Error: "not found"}}, nil
		},
		"ok_ptr_nilerr": func(_ context.Context, _ string, _ json.RawMessage) (any, error) { return ptrRejectResult{}, nil },
	}

	for _, action := range []string{"ok_map", "ok_envelope", "reject_kind", "reject_nokind", "boom", "reject_ptr", "ok_ptr_nilerr", "nonexistent"} {
		dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{Action: action, Project: "p"})
	}

	if len(got) != 8 {
		t.Fatalf("observer fired %d times, want 8", len(got))
	}
	byAction := map[string]string{}
	for _, c := range got {
		byAction[c.action] = c.errClass
		if c.project != "p" {
			t.Errorf("action %q: project = %q, want p", c.action, c.project)
		}
	}
	want := map[string]string{
		"ok_map":        "",
		"ok_envelope":   "",
		"reject_kind":   "missing_required",
		"reject_nokind": "rejected",
		"boom":          "handler_error",
		"reject_ptr":    "rejected",
		"ok_ptr_nilerr": "",
		"nonexistent":   "unknown_action",
	}
	for action, w := range want {
		if got := byAction[action]; got != w {
			t.Errorf("action %q: error_class = %q, want %q", action, got, w)
		}
	}
}

// TestDispatch_CallObserver_NilIsNoop confirms dispatch works with no
// observer set (tests / bare use): capture is zero-cost and never panics.
func TestDispatch_CallObserver_NilIsNoop(t *testing.T) {
	dispatch.SetCallObserver(nil)
	table := dispatch.Table{"a": func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return map[string]string{"ok": "1"}, nil
	}}
	if _, _, err := dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{Action: "a"}); err != nil {
		t.Fatalf("dispatch with nil observer: %v", err)
	}
}
