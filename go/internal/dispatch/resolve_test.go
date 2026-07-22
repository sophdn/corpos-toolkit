package dispatch_test

import (
	"context"
	"encoding/json"
	"testing"

	"toolkit/internal/dispatch"
)

// captureProjectHandler records the project the dispatcher threads into the
// handler, so a test can assert on the resolved scope.
func captureProjectHandler(dst *string, hit *bool) dispatch.Handler {
	return func(_ context.Context, project string, _ json.RawMessage) (any, error) {
		*dst = project
		*hit = true
		return map[string]string{"ok": "1"}, nil
	}
}

func TestNewCwdProjectResolver(t *testing.T) {
	paths := []dispatch.ProjectPath{
		{ID: "corpos-toolkit", Path: "/home/user/dev/corpos-toolkit"},
		{ID: "corpos", Path: "/home/user/dev/corpos"},
	}
	r := dispatch.NewCwdProjectResolver(paths, "session-default")

	cases := []struct {
		name string
		args dispatch.Args
		want string
	}{
		{"explicit project wins", dispatch.Args{Project: "explicit"}, "explicit"},
		{"cwd exact match", dispatch.Args{Cwd: "/home/user/dev/corpos"}, "corpos"},
		{"cwd descendant match", dispatch.Args{Cwd: "/home/user/dev/corpos-toolkit/go"}, "corpos-toolkit"},
		{"cwd sibling does not falsely match", dispatch.Args{Cwd: "/home/user/dev/corpos-lab"}, "session-default"},
		{"no cwd falls back to default", dispatch.Args{}, "session-default"},
	}
	for _, tc := range cases {
		if got := r(tc.args); got != tc.want {
			t.Errorf("%s: resolver=%q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestNewCwdProjectResolver_PreferenceOrder migrates the resolver preference
// matrix from cmd/toolkit-server (where this logic used to live) now that both
// transports share one implementation. Paths are pre-sorted longest-first, as
// loadProjectPaths guarantees, so overlapping prefixes resolve deterministically.
func TestNewCwdProjectResolver_PreferenceOrder(t *testing.T) {
	paths := []dispatch.ProjectPath{
		{ID: "mcp-servers", Path: "/home/user/dev/mcp-servers"},
		{ID: "self-compile", Path: "/home/user/dev/self-compile"},
		{ID: "dev-umbrella", Path: "/home/user/dev"},
	}
	r := dispatch.NewCwdProjectResolver(paths, "fallback")
	cases := []struct {
		name string
		args dispatch.Args
		want string
	}{
		{"explicit project wins", dispatch.Args{Project: "override", Cwd: "/home/user/dev/mcp-servers"}, "override"},
		{"cwd matches longest prefix", dispatch.Args{Cwd: "/home/user/dev/mcp-servers/go"}, "mcp-servers"},
		{"cwd exact path match", dispatch.Args{Cwd: "/home/user/dev/self-compile"}, "self-compile"},
		{"cwd not under any registered path falls back", dispatch.Args{Cwd: "/tmp/somewhere"}, "fallback"},
		{"empty cwd falls back to flag default", dispatch.Args{}, "fallback"},
		{"prefix collision picks longest", dispatch.Args{Cwd: "/home/user/dev/self-compile/world"}, "self-compile"},
		{"cwd is parent project when no deeper match", dispatch.Args{Cwd: "/home/user/dev/other"}, "dev-umbrella"},
	}
	for _, tc := range cases {
		if got := r(tc.args); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHasPathPrefix_RejectsPartialDirName pins the separator boundary check —
// /home/user/dev-other must NOT match /home/user/dev.
func TestHasPathPrefix_RejectsPartialDirName(t *testing.T) {
	if dispatch.HasPathPrefix("/home/user/dev-other", "/home/user/dev") {
		t.Error("dev-other incorrectly matched /home/user/dev")
	}
	if !dispatch.HasPathPrefix("/home/user/dev", "/home/user/dev") {
		t.Error("exact match should succeed")
	}
	if !dispatch.HasPathPrefix("/home/user/dev/x", "/home/user/dev") {
		t.Error("nested path should match")
	}
}

// TestNewCwdProjectResolver_EmptyDefaultFailsLoud pins that with no explicit
// project, no CWD match, and an empty default, the resolver returns "" — for a
// project-scoped write the handler then surfaces its "requires a project"
// error rather than silently misfiling into a static default project.
func TestNewCwdProjectResolver_EmptyDefaultFailsLoud(t *testing.T) {
	r := dispatch.NewCwdProjectResolver(nil, "")
	if got := r(dispatch.Args{Cwd: "/tmp/nowhere"}); got != "" {
		t.Errorf("resolver=%q, want \"\" (fail loud)", got)
	}
}

func TestIsCrossProjectRead(t *testing.T) {
	reads := []string{"bug_list", "task_search", "suggestion_list", "chain_status", "roadmap_list"}
	for _, a := range reads {
		if !dispatch.IsCrossProjectRead("work", a) {
			t.Errorf("work.%s should be a cross-project read", a)
		}
	}
	writes := []string{"forge", "record", "bug_resolve", "task_start", "roadmap_update", "task_unstart"}
	for _, a := range writes {
		if dispatch.IsCrossProjectRead("work", a) {
			t.Errorf("work.%s is mutating and must NOT be a cross-project read", a)
		}
	}
	// Surface-qualified: a bug_list on a different surface is not the work read.
	if dispatch.IsCrossProjectRead("measure", "bug_list") {
		t.Error("measure.bug_list must not match the work read allowlist")
	}
}

// TestDispatchWithOptions_ReadActionIsCrossProjectDespiteDefault is the
// regression for bug
// read-actions-inherit-session-default-project-breaking-cross-project-read-contract:
// an unscoped read must reach the handler with project="" (cross-project),
// NOT the resolver's default project.
func TestDispatchWithOptions_ReadActionIsCrossProjectDespiteDefault(t *testing.T) {
	var seen string
	var hit bool
	table := dispatch.Table{"bug_list": captureProjectHandler(&seen, &hit)}
	resolver := dispatch.StaticProjectResolver("seed-packet")

	_, _, err := dispatch.DispatchWithOptions(context.Background(), resolver, table,
		dispatch.Args{Action: "bug_list"}, dispatch.Options{Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !hit {
		t.Fatal("handler never ran")
	}
	if seen != "" {
		t.Errorf("unscoped read reached handler with project=%q, want \"\" (cross-project)", seen)
	}
}

// TestDispatchWithOptions_ExplicitProjectScopesRead pins that an explicit
// project still narrows a read — the cross-project default only applies when
// no scope was supplied.
func TestDispatchWithOptions_ExplicitProjectScopesRead(t *testing.T) {
	var seen string
	var hit bool
	table := dispatch.Table{"bug_list": captureProjectHandler(&seen, &hit)}
	resolver := dispatch.StaticProjectResolver("seed-packet")

	_, _, err := dispatch.DispatchWithOptions(context.Background(), resolver, table,
		dispatch.Args{Action: "bug_list", Project: "corpos"}, dispatch.Options{Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seen != "corpos" {
		t.Errorf("explicit-scoped read reached handler with project=%q, want \"corpos\"", seen)
	}
}

// TestDispatchWithOptions_WriteActionKeepsDefaultProject pins that the read
// exemption does NOT leak to writes: an unscoped mutating action still inherits
// the resolver's default project so create-writes land somewhere.
func TestDispatchWithOptions_WriteActionKeepsDefaultProject(t *testing.T) {
	var seen string
	var hit bool
	table := dispatch.Table{"forge": captureProjectHandler(&seen, &hit)}
	resolver := dispatch.StaticProjectResolver("session-default")

	_, _, err := dispatch.DispatchWithOptions(context.Background(), resolver, table,
		dispatch.Args{Action: "forge"}, dispatch.Options{Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seen != "session-default" {
		t.Errorf("unscoped write reached handler with project=%q, want \"session-default\"", seen)
	}
}
