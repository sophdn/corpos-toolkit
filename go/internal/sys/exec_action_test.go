package sys

// exec_action_test.go is the net for the gated sys.exec action — the allowlist
// half of the security gate (the rationale half is enforced at the dispatch
// boundary via dispatch-policy.toml). See docs/OWNED_EXEC_SECURITY.md.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandAllowlist_Permit(t *testing.T) {
	a := defaultAllowlist()
	cases := []struct {
		command   string
		wantAllow bool
	}{
		{"git status", true},
		{"go test ./...", true},
		{"cat a.txt | grep needle", true}, // every head allowlisted
		{"cd sub && git log", true},       // operator split, both heads ok
		{"FOO=bar git log", true},         // leading env assignment skipped
		{"/usr/bin/git status", true},     // basename resolved
		{"rm -rf /tmp/x", false},          // rm not allowlisted
		{"git status && rm -rf x", false}, // one bad head fails the whole command
		{"bash -c 'rm -rf /'", false},     // bash not allowlisted (would defeat the gate)
		{"echo $(whoami)", false},         // command substitution rejected
		{"echo `id`", false},              // backtick substitution rejected
		{"", false},                       // empty
		{"   ", false},                    // whitespace only
	}
	for _, c := range cases {
		err := a.permit(c.command)
		if (err == nil) != c.wantAllow {
			t.Errorf("permit(%q) err=%v, wantAllow=%v", c.command, err, c.wantAllow)
		}
	}
}

func TestLoadAllowlist_EnvExtends(t *testing.T) {
	a := loadAllowlist("mytool, otherthing")
	if err := a.permit("mytool --flag"); err != nil {
		t.Errorf("env-added tool rejected: %v", err)
	}
	if err := a.permit("otherthing run"); err != nil {
		t.Errorf("env-added tool rejected: %v", err)
	}
	// default entries still present
	if err := a.permit("git status"); err != nil {
		t.Errorf("default tool rejected after env extend: %v", err)
	}
	// empty env → default only
	b := loadAllowlist("")
	if err := b.permit("git status"); err != nil {
		t.Errorf("default rejected with empty env: %v", err)
	}
	if err := b.permit("mytool x"); err == nil {
		t.Error("non-default tool allowed with empty env")
	}
}

func TestCommandHeads(t *testing.T) {
	cases := []struct {
		command string
		want    []string
	}{
		{"git status", []string{"git"}},
		{"cd sub && git log", []string{"cd", "git"}},
		{"cat a | grep b", []string{"cat", "grep"}},
		{"FOO=1 BAR=2 make build", []string{"make"}},
		{"a; b; c", []string{"a", "b", "c"}},
		{"git log > out.txt", []string{"git"}},
	}
	for _, c := range cases {
		got := commandHeads(c.command)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("commandHeads(%q) = %v, want %v", c.command, got, c.want)
		}
	}
}

func newTestExecService(t *testing.T) *execService {
	t.Helper()
	r, err := NewRunner(t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return &execService{runner: r, allow: defaultAllowlist()}
}

func TestHandleExec_AllowedRuns(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: "echo hi"})
	res, err := s.handle(context.Background(), raw)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.Output != "hi" || res.ExitCode != 0 {
		t.Errorf("res = %+v, want output hi exit 0", res)
	}
}

func TestHandleExec_RejectedCommand(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: "rm -rf /tmp/whatever"})
	_, err := s.handle(context.Background(), raw)
	if err == nil {
		t.Fatal("rejected command returned nil error")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error = %v, want allowlist rejection", err)
	}
}

func TestHandleExec_EmptyCommand(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: ""})
	if _, err := s.handle(context.Background(), raw); err == nil {
		t.Error("empty command = nil error, want error")
	}
}

func TestHandleExec_SubstitutionRejected(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: "echo $(rm -rf /)"})
	if _, err := s.handle(context.Background(), raw); err == nil {
		t.Error("command substitution = nil error, want rejection")
	}
}

func TestHandleExec_ExitCodePropagates(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: "test -f /this/does/not/exist"})
	res, err := s.handle(context.Background(), raw)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if res.ExitCode == 0 {
		t.Error("expected non-zero exit for failing test command")
	}
}

func TestHandleExec_InvalidParams(t *testing.T) {
	s := newTestExecService(t)
	if _, err := s.handle(context.Background(), []byte(`{"timeout_ms":"nope"}`)); err == nil {
		t.Error("invalid params = nil error, want error")
	}
}

func TestHandleExec_UnavailableSandboxFailsClosed(t *testing.T) {
	s := newTestExecService(t)
	raw, _ := json.Marshal(ExecParams{Command: "echo hi", Sandbox: "bogus-backend"})
	if _, err := s.handle(context.Background(), raw); err == nil {
		t.Error("unknown sandbox = nil error, want fail-closed error")
	}
}

func TestBuildTable_HasIntrospectionOnly(t *testing.T) {
	tbl := BuildTable()
	for _, name := range []string{"ps", "ports", "units", "containers"} {
		if _, ok := tbl[name]; !ok {
			t.Errorf("BuildTable missing introspection action %q", name)
		}
	}
	// exec is RETIRED from the surface (T6) — corpos owns it natively. The
	// implementation is retained as the parity oracle but no longer registered.
	if _, ok := tbl["exec"]; ok {
		t.Error("BuildTable should NOT register exec (retired in T6)")
	}
}

func TestIsEnvAssignment(t *testing.T) {
	yes := []string{"FOO=bar", "A=", "x_1=v"}
	no := []string{"git", "=bad", "a-b=c", "/usr/bin/x"}
	for _, s := range yes {
		if !isEnvAssignment(s) {
			t.Errorf("isEnvAssignment(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if isEnvAssignment(s) {
			t.Errorf("isEnvAssignment(%q) = true, want false", s)
		}
	}
}
