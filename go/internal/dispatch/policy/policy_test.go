package policy_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/dispatch/policy"
)

// writePolicy writes a TOML body to a temp file and returns its path.
func writePolicy(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dispatch-policy.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func TestLoad_EmptyPathReturnsEmptyRegistry(t *testing.T) {
	r, err := policy.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 0 {
		t.Errorf("empty registry: len=%d, want 0", r.Len())
	}
}

func TestLoad_MissingFileReturnsEmptyRegistry(t *testing.T) {
	r, err := policy.Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 0 {
		t.Errorf("missing-file registry: len=%d, want 0", r.Len())
	}
}

func TestLoad_ParsesSurfaceActionTables(t *testing.T) {
	path := writePolicy(t, `
[work.bug_resolve]
requires_rationale = true

[work.bug_read]
requires_rationale = false

[knowledge.library_add]
requires_rationale = true
`)
	r, err := policy.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := r.Gates("work", "bug_resolve").RequiresRationale; got != true {
		t.Errorf("work.bug_resolve: requires_rationale=%v, want true", got)
	}
	if got := r.Gates("work", "bug_read").RequiresRationale; got != false {
		t.Errorf("work.bug_read: requires_rationale=%v, want false", got)
	}
	if got := r.Gates("knowledge", "library_add").RequiresRationale; got != true {
		t.Errorf("knowledge.library_add: requires_rationale=%v, want true", got)
	}
}

func TestLoad_UnknownActionReturnsZeroGates(t *testing.T) {
	path := writePolicy(t, `[work.bug_resolve]
requires_rationale = true
`)
	r, _ := policy.Load(path)
	if got := r.Gates("work", "task_search").RequiresRationale; got != false {
		t.Errorf("unknown action: requires_rationale=%v, want false (zero default)", got)
	}
}

func TestLoad_MalformedTomlReturnsError(t *testing.T) {
	path := writePolicy(t, "this is not [valid toml = {")
	_, err := policy.Load(path)
	if err == nil {
		t.Fatal("Load: expected error on malformed TOML, got nil")
	}
}

func TestGates_NilRegistryYieldsZero(t *testing.T) {
	var r *policy.Registry
	if got := r.Gates("work", "bug_resolve").RequiresRationale; got != false {
		t.Errorf("nil registry: requires_rationale=%v, want false", got)
	}
}

// --- ValidateRationale ---

func TestValidateRationale_NoGateNoCheck(t *testing.T) {
	g := policy.Gates{RequiresRationale: false}
	// Even empty + agent passes when the gate is off.
	if err := g.ValidateRationale("agent", ""); err != nil {
		t.Errorf("no-gate empty/agent: got %v, want nil", err)
	}
	if err := g.ValidateRationale("agent", "ok"); err != nil {
		t.Errorf("no-gate boilerplate/agent: got %v, want nil", err)
	}
}

func TestValidateRationale_HumanActorPassesThrough(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	// Empty rationale: humans pass.
	if err := g.ValidateRationale("human", ""); err != nil {
		t.Errorf("human empty: got %v, want nil", err)
	}
	// Boilerplate: humans pass — no stop-list for non-agents.
	if err := g.ValidateRationale("human", "ok"); err != nil {
		t.Errorf("human boilerplate: got %v, want nil", err)
	}
	// Short: humans pass.
	if err := g.ValidateRationale("human", "x"); err != nil {
		t.Errorf("human short: got %v, want nil", err)
	}
}

func TestValidateRationale_SystemActorPassesThrough(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	if err := g.ValidateRationale("system", ""); err != nil {
		t.Errorf("system empty: got %v, want nil", err)
	}
}

func TestValidateRationale_AgentEmptyRejected(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	err := g.ValidateRationale("agent", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var re *policy.RationaleError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RationaleError, got %T", err)
	}
	if re.Field != "rationale" {
		t.Errorf("Field=%q, want rationale", re.Field)
	}
	if re.Reason != "empty" {
		t.Errorf("Reason=%q, want empty", re.Reason)
	}
	if re.Hint == "" {
		t.Errorf("Hint is empty")
	}
}

func TestValidateRationale_AgentWhitespaceRejected(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	cases := []string{
		" ",
		"\t",
		"\n",
		"  \t \n  ",
	}
	for _, in := range cases {
		err := g.ValidateRationale("agent", in)
		var re *policy.RationaleError
		if !errors.As(err, &re) || re.Reason != "empty" {
			t.Errorf("whitespace %q: got %v, want empty-rejection", in, err)
		}
	}
}

func TestValidateRationale_AgentTooShortRejected(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	// 5 chars after trim — below MinRationaleLen (6).
	err := g.ValidateRationale("agent", "hello")
	var re *policy.RationaleError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RationaleError, got %v", err)
	}
	if re.Reason != "too short" {
		t.Errorf("Reason=%q, want too short", re.Reason)
	}
}

func TestValidateRationale_AgentBoilerplateRejected(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	// These all clear the 6-char minimum but match the stop-list.
	cases := []string{
		"as requested",
		"as asked",
		"see above",
		"see below",
		"complete",
		"completed",
		"because",
		"testing",
		"AS REQUESTED", // case-insensitive
		"  as requested  ",
	}
	for _, in := range cases {
		err := g.ValidateRationale("agent", in)
		var re *policy.RationaleError
		if !errors.As(err, &re) {
			t.Errorf("boilerplate %q: got %v, want *RationaleError", in, err)
			continue
		}
		if re.Reason != "boilerplate" {
			t.Errorf("boilerplate %q: Reason=%q, want boilerplate", in, re.Reason)
		}
	}
}

func TestValidateRationale_AgentSubstantiveShortPasses(t *testing.T) {
	g := policy.Gates{RequiresRationale: true}
	// 6+ chars, not on stop-list — passes.
	cases := []string{
		"fixing",
		"fix typo in error message",
		"merge conflict resolved",
		"unblock CI",
	}
	for _, in := range cases {
		if err := g.ValidateRationale("agent", in); err != nil {
			t.Errorf("substantive %q: got %v, want nil", in, err)
		}
	}
}

func TestRationaleError_ErrorString(t *testing.T) {
	e := &policy.RationaleError{Field: "rationale", Reason: "empty"}
	if !strings.Contains(e.Error(), "empty") {
		t.Errorf("Error()=%q, want it to mention reason", e.Error())
	}
}

// TestRepoPolicyFile_LoadsCleanly is the smoke test against the actual
// shipped action-manifests/dispatch-policy.toml. Catches accidental syntax
// breakage at PR time without needing to wire a separate validation step.
func TestRepoPolicyFile_LoadsCleanly(t *testing.T) {
	// Test runs from the package dir (go/internal/dispatch/policy) so the
	// repo root is four levels up.
	path := filepath.Join("..", "..", "..", "..", "action-manifests", "dispatch-policy.toml")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if _, statErr := os.Stat(abs); statErr != nil {
		t.Skipf("policy file not at %s — skipping repo-file smoke test (%v)", abs, statErr)
	}
	r, err := policy.Load(abs)
	if err != nil {
		t.Fatalf("Load repo policy: %v", err)
	}
	if r.Len() == 0 {
		t.Error("repo policy loaded but Len()==0 — TOML may have a structure error")
	}
	// Pin a few canonical entries from the T3 acceptance list.
	mustGate := []struct {
		surface, action string
	}{
		{"work", "bug_resolve"},
		{"work", "bug_reopen"},
		{"work", "task_start"},
		{"work", "task_complete"},
		{"work", "task_cancel"},
		{"work", "task_reopen"},
		{"work", "task_block"},
		{"work", "task_unblock"},
		{"work", "task_edit"},
		{"work", "chain_close"},
		{"work", "forge"},
		{"work", "forge_edit"},
		{"work", "forge_delete"},
	}
	for _, m := range mustGate {
		if !r.Gates(m.surface, m.action).RequiresRationale {
			t.Errorf("%s.%s: requires_rationale should be true per T3 acceptance criteria", m.surface, m.action)
		}
	}
	// Pin a few read-only actions that should NOT gate.
	mustNotGate := []struct {
		surface, action string
	}{
		{"work", "bug_read"},
		{"work", "bug_list"},
		{"work", "task_read"},
		{"work", "chain_status"},
	}
	for _, m := range mustNotGate {
		if r.Gates(m.surface, m.action).RequiresRationale {
			t.Errorf("%s.%s: requires_rationale should be false (read-only)", m.surface, m.action)
		}
	}
}
