package dispatch_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolkit/internal/dispatch"
	"toolkit/internal/dispatch/policy"
	"toolkit/internal/events"
)

// stubHandler returns the action name so tests can distinguish handler hits.
func stubHandler(name string) dispatch.Handler {
	return func(_ context.Context, _ string, _ json.RawMessage) (any, error) {
		return map[string]string{"hit": name}, nil
	}
}

// decodeResult pulls the JSON-encoded payload out of the MCP TextContent
// envelope back into a generic map. Tests assert on this, not on the
// CallToolResult shape.
func decodeResult(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestDispatch_IntrospectActionReturnsSortedActionList(t *testing.T) {
	table := dispatch.Table{
		"chain_status": stubHandler("chain_status"),
		"bug_list":     stubHandler("bug_list"),
		"task_read":    stubHandler("task_read"),
	}
	res, _, err := dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{
		Action: dispatch.IntrospectAction,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	actions, ok := payload["actions"].([]any)
	if !ok {
		t.Fatalf("introspect payload missing actions array: %+v", payload)
	}
	want := []string{"bug_list", "chain_status", "task_read"}
	if len(actions) != len(want) {
		t.Fatalf("introspect: got %d actions, want %d (%+v)", len(actions), len(want), actions)
	}
	for i, w := range want {
		if actions[i] != w {
			t.Errorf("introspect[%d]: got %v, want %q", i, actions[i], w)
		}
	}
}

func TestDispatch_UnknownActionEnclosesSupportedList(t *testing.T) {
	table := dispatch.Table{
		"chain_status": stubHandler("chain_status"),
		"bug_list":     stubHandler("bug_list"),
	}
	res, _, err := dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{
		Action: "list_chains",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	if payload["error"] != "action not implemented" {
		t.Errorf("error field: got %v", payload["error"])
	}
	if payload["action"] != "list_chains" {
		t.Errorf("action field: got %v", payload["action"])
	}
	supported, ok := payload["supported"].([]any)
	if !ok {
		t.Fatalf("unknown-action error missing supported list: %+v", payload)
	}
	want := []string{"bug_list", "chain_status"}
	if len(supported) != len(want) {
		t.Fatalf("supported: got %d, want %d (%+v)", len(supported), len(want), supported)
	}
	for i, w := range want {
		if supported[i] != w {
			t.Errorf("supported[%d]: got %v, want %q", i, supported[i], w)
		}
	}
}

func TestDispatch_KnownActionStillReachesHandler(t *testing.T) {
	// Regression: the introspection short-circuit must not eat valid
	// action calls. Verify a real action still dispatches.
	table := dispatch.Table{
		"chain_status": stubHandler("chain_status"),
	}
	res, _, err := dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{
		Action: "chain_status",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	if payload["hit"] != "chain_status" {
		t.Errorf("handler not reached: %+v", payload)
	}
}

func TestDispatch_IntrospectOnEmptyTableReturnsEmptyList(t *testing.T) {
	table := dispatch.Table{}
	res, _, err := dispatch.Dispatch(context.Background(), "p", table, dispatch.Args{
		Action: dispatch.IntrospectAction,
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	actions, ok := payload["actions"].([]any)
	if !ok {
		t.Fatalf("introspect payload missing actions array: %+v", payload)
	}
	if len(actions) != 0 {
		t.Errorf("empty table: got %d actions, want 0 (%+v)", len(actions), actions)
	}
}

// TestDispatchWith_ResolverSeesArgsAndThreadsProjectToHandler pins bug 1311:
// DispatchWith calls the resolver with the full Args (so it can branch on
// Cwd as well as Project) and threads the resolved string into the handler's
// project parameter.
func TestDispatchWith_ResolverSeesArgsAndThreadsProjectToHandler(t *testing.T) {
	var seenProject string
	table := dispatch.Table{
		"echo": func(_ context.Context, project string, _ json.RawMessage) (any, error) {
			seenProject = project
			return map[string]string{"project": project}, nil
		},
	}
	resolver := func(args dispatch.Args) string {
		if args.Cwd == "/dev/mcp-servers" {
			return "mcp-servers"
		}
		return ""
	}
	_, _, err := dispatch.DispatchWith(context.Background(), resolver, table, dispatch.Args{
		Action: "echo",
		Cwd:    "/dev/mcp-servers",
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seenProject != "mcp-servers" {
		t.Errorf("handler saw project=%q, want %q", seenProject, "mcp-servers")
	}
}

// TestStaticProjectResolver_PerCallProjectWinsOverDefault pins that an
// explicit args.Project overrides the configured default.
func TestStaticProjectResolver_PerCallProjectWinsOverDefault(t *testing.T) {
	r := dispatch.StaticProjectResolver("default-proj")
	if got := r(dispatch.Args{Project: "per-call"}); got != "per-call" {
		t.Errorf("per-call: got %q, want %q", got, "per-call")
	}
	if got := r(dispatch.Args{}); got != "default-proj" {
		t.Errorf("default: got %q, want %q", got, "default-proj")
	}
}

// TestDispatchWith_ParamsNestedProjectHonoredOverDefault is the regression
// test for bug 1070: a caller that nests `project` inside params (instead of
// passing it as the top-level dispatch arg) must NOT have it silently dropped
// and replaced by the server's --default-project. The nested intent is clear,
// so it is honored: the handler sees the params-nested project, not the
// default. This guards against the confidently-wrong-empty failure where a
// cross-project-intended query silently narrowed to the default project.
func TestDispatchWith_ParamsNestedProjectHonoredOverDefault(t *testing.T) {
	var seenProject string
	table := dispatch.Table{
		"echo": func(_ context.Context, project string, _ json.RawMessage) (any, error) {
			seenProject = project
			return map[string]string{"project": project}, nil
		},
	}
	// Server configured with a default project (the seed-packet trap).
	resolver := dispatch.StaticProjectResolver("seed-packet")
	_, _, err := dispatch.DispatchWith(context.Background(), resolver, table, dispatch.Args{
		Action: "echo",
		// project NESTED inside params, top-level Project empty.
		Params: map[string]any{"status": "open", "project": "corpos"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seenProject != "corpos" {
		t.Errorf("handler saw project=%q, want %q (params-nested project must be honored, not silently dropped to the --default-project)", seenProject, "corpos")
	}
}

// TestDispatchWith_TopLevelProjectWinsOverParamsNested pins that an explicit
// top-level project still takes precedence over a params-nested one — the
// envelope field is the canonical scope; the params promotion (bug 1070) only
// fills in when the top-level is empty.
func TestDispatchWith_TopLevelProjectWinsOverParamsNested(t *testing.T) {
	var seenProject string
	table := dispatch.Table{
		"echo": func(_ context.Context, project string, _ json.RawMessage) (any, error) {
			seenProject = project
			return map[string]string{"project": project}, nil
		},
	}
	resolver := dispatch.StaticProjectResolver("seed-packet")
	_, _, err := dispatch.DispatchWith(context.Background(), resolver, table, dispatch.Args{
		Action:  "echo",
		Project: "top-level-proj",
		Params:  map[string]any{"project": "params-proj"},
	})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if seenProject != "top-level-proj" {
		t.Errorf("handler saw project=%q, want %q (top-level project must win over params-nested)", seenProject, "top-level-proj")
	}
}

// TestMetaToolInputSchema_ParamsTypeIsStringNotArray locks in the fix for
// the Claude-Zod-rejects-type-array regression. The validator rejects
// schemas where a `type` field is a JSON array (e.g. ["object", "null"]),
// silently dropping every meta-tool from the session. Params absence is
// already handled by `required: ["action"]` + the `omitempty` struct tag,
// so the null branch is redundant.
func TestMetaToolInputSchema_ParamsTypeIsStringNotArray(t *testing.T) {
	schema := dispatch.MetaToolInputSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties missing or wrong type: %+v", schema["properties"])
	}
	params, ok := props["params"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties.params missing or wrong type: %+v", props["params"])
	}
	got, ok := params["type"].(string)
	if !ok {
		t.Fatalf("params.type must be a string, got %T (%+v) — a type-array trips the Claude Zod validator", params["type"], params["type"])
	}
	if got != "object" {
		t.Errorf("params.type = %q, want \"object\"", got)
	}
}

// TestMetaToolInputSchema_AdvertisesCwd pins that the meta-tool schema
// exposes the cwd field added for bug 1311's CWD-derived project resolution.
func TestMetaToolInputSchema_AdvertisesCwd(t *testing.T) {
	schema := dispatch.MetaToolInputSchema()
	props := schema["properties"].(map[string]any)
	if _, ok := props["cwd"]; !ok {
		t.Errorf("schema.properties.cwd missing: %+v", props)
	}
}

// TestMetaToolInputSchema_AdvertisesRationale pins that the meta-tool
// schema exposes the rationale field added by T3 of the
// agent-first-substrate chain. Without this advertisement the Zod
// validator on the MCP client side would reject unknown top-level keys.
func TestMetaToolInputSchema_AdvertisesRationale(t *testing.T) {
	schema := dispatch.MetaToolInputSchema()
	props := schema["properties"].(map[string]any)
	r, ok := props["rationale"]
	if !ok {
		t.Fatalf("schema.properties.rationale missing: %+v", props)
	}
	rmap, ok := r.(map[string]any)
	if !ok {
		t.Fatalf("rationale schema: got %T, want map[string]any", r)
	}
	if rmap["type"] != "string" {
		t.Errorf("rationale.type = %v, want string", rmap["type"])
	}
}

// --- Rationale enforcement (chain agent-first-substrate T3) ---

// loadPolicyFromBody parses an inline TOML body into a *policy.Registry.
// Used to build test-scoped policy without depending on the repo file.
func loadPolicyFromBody(t *testing.T, body string) *policy.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "p.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := policy.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

// agentCtx returns a context with an agent actor attached.
func agentCtx() context.Context {
	return events.WithActor(context.Background(), events.Actor{Kind: "agent", ID: "test-agent"})
}

// humanCtx returns a context with a human actor attached.
func humanCtx() context.Context {
	return events.WithActor(context.Background(), events.Actor{Kind: "human", ID: "portal-test"})
}

// systemCtx returns a context with a system actor attached.
func systemCtx() context.Context {
	return events.WithActor(context.Background(), events.Actor{Kind: "system", ID: "cli-test"})
}

func TestDispatchWithOptions_GatedAgentEmptyRationaleRejected(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `
[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{
		"task_complete": stubHandler("task_complete"),
	}
	res, _, err := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	if payload["error"] != "rationale_required" {
		t.Errorf("error=%v, want rationale_required", payload["error"])
	}
	if payload["field"] != "rationale" {
		t.Errorf("field=%v, want rationale", payload["field"])
	}
	if payload["reason"] != "empty" {
		t.Errorf("reason=%v, want empty", payload["reason"])
	}
	if payload["action"] != "task_complete" {
		t.Errorf("action=%v, want task_complete", payload["action"])
	}
	if payload["surface"] != "work" {
		t.Errorf("surface=%v, want work", payload["surface"])
	}
	if _, ok := payload["hint"]; !ok {
		t.Error("hint missing from envelope")
	}
}

// Bug 1403: when the caller supplies a non-empty rationale inside
// params (a common authoring mistake — rationale is envelope-level),
// the dispatcher should reject with reason="wrong_nesting" instead of
// the generic "empty" so the agent knows to move the field, not write
// new text.
func TestDispatchWithOptions_GatedAgentRationaleNestedInParams(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, err := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
		Params: map[string]any{
			"id":        42,
			"rationale": "Closing because the work landed in commit abc123.",
		},
		// Rationale envelope field deliberately empty.
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	if payload["error"] != "rationale_required" {
		t.Errorf("error=%v, want rationale_required", payload["error"])
	}
	if payload["reason"] != "wrong_nesting" {
		t.Errorf("reason=%v, want wrong_nesting", payload["reason"])
	}
	hint, _ := payload["hint"].(string)
	if hint == "" {
		t.Error("hint missing from envelope")
	}
}

// Bug 1448 regression guard: the wrong_nesting detection in the
// dispatcher's rationale gate is action-agnostic — it must fire for
// task_start the same way it fires for task_complete. Filed because an
// agent observed asymmetric envelopes in the wild (probably stdio
// binary staleness pre-bug-1403); this pins the parity so the gate
// can't quietly regress to per-action handling.
func TestDispatchWithOptions_GatedAgentRationaleNestedInParams_TaskStart(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_start]
requires_rationale = true
`)
	table := dispatch.Table{"task_start": stubHandler("task_start")}
	res, _, err := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_start",
		Params: map[string]any{
			"slug":      "some-task",
			"rationale": "Starting because the predecessor task closed.",
		},
		// Rationale envelope field deliberately empty.
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload := decodeResult(t, res)
	if payload["error"] != "rationale_required" {
		t.Errorf("error=%v, want rationale_required", payload["error"])
	}
	if payload["reason"] != "wrong_nesting" {
		t.Errorf("reason=%v, want wrong_nesting", payload["reason"])
	}
	if payload["action"] != "task_start" {
		t.Errorf("action=%v, want task_start", payload["action"])
	}
	hint, _ := payload["hint"].(string)
	if hint == "" {
		t.Error("hint missing from envelope")
	}
	if !strings.Contains(hint, "envelope") {
		t.Errorf("hint %q does not name the envelope-vs-params distinction", hint)
	}
}

// Bug 1403 regression guard: a genuinely-empty rationale (no nested
// fallback either) still reports reason="empty".
func TestDispatchWithOptions_GatedAgentEmptyRationaleStillEmpty(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
		Params: map[string]any{"id": 42}, // no rationale anywhere
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["reason"] != "empty" {
		t.Errorf("reason=%v, want empty for truly-absent rationale", payload["reason"])
	}
}

func TestDispatchWithOptions_GatedAgentWhitespaceRejected(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action:    "task_complete",
		Rationale: "   \t\n   ",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["reason"] != "empty" {
		t.Errorf("whitespace rejected as %v, want empty", payload["reason"])
	}
}

func TestDispatchWithOptions_GatedAgentBoilerplateRejected(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action:    "task_complete",
		Rationale: "as requested",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["reason"] != "boilerplate" {
		t.Errorf("boilerplate rejected as %v, want boilerplate", payload["reason"])
	}
}

func TestDispatchWithOptions_GatedAgentShortRejected(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action:    "task_complete",
		Rationale: "ok!", // 3 chars, below 6-char minimum
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["reason"] != "too short" {
		t.Errorf("short rejected as %v, want too short", payload["reason"])
	}
}

func TestDispatchWithOptions_GatedAgentSubstantivePasses(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action:    "task_complete",
		Rationale: "all subtasks closed and handoff written",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["hit"] != "task_complete" {
		t.Errorf("substantive rationale: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_GatedHumanEmptyPasses(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(humanCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["hit"] != "task_complete" {
		t.Errorf("human empty: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_GatedSystemEmptyPasses(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(systemCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["hit"] != "task_complete" {
		t.Errorf("system empty: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_ReadOnlyActionUnaffected(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
# bug_read intentionally absent — read-only, no gate.
`)
	table := dispatch.Table{
		"task_complete": stubHandler("task_complete"),
		"bug_read":      stubHandler("bug_read"),
	}
	// Agent calling read-only action with NO rationale must pass.
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "bug_read",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["hit"] != "bug_read" {
		t.Errorf("read-only agent: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_NilPolicyDisablesEnforcement(t *testing.T) {
	// Even a gated action passes when Policy is nil — degraded-mode
	// boot must not break agent calls.
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	}, dispatch.Options{Policy: nil, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["hit"] != "task_complete" {
		t.Errorf("nil policy: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_RationaleStampedOntoCtx(t *testing.T) {
	// Handler captures the rationale from ctx; the dispatcher must
	// stamp it after the gate passes so events.Emit can pick it up.
	var seen string
	table := dispatch.Table{
		"task_complete": func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
			seen = events.RationaleFromContext(ctx)
			return map[string]string{"ok": "yes"}, nil
		},
	}
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	const want = "shipping the closing commit on this task"
	_, _, _ = dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action:    "task_complete",
		Rationale: want,
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	if seen != want {
		t.Errorf("rationale on ctx: got %q, want %q", seen, want)
	}
}

// TestDispatchWithOptions_UnknownActionStillReports verifies the unknown-
// action error envelope path runs even when the policy gate would have
// rejected an empty rationale — unknown-action should win because the
// action doesn't exist to gate.
func TestDispatchWithOptions_UnknownActionStillReports(t *testing.T) {
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "nonexistent_action",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	if payload["error"] != "action not implemented" {
		t.Errorf("unknown action: error=%v, want 'action not implemented'", payload["error"])
	}
}

func TestDispatchWithOptions_LegacyDispatchWithStillWorks(t *testing.T) {
	// Bare DispatchWith (no options) must behave as before T3 —
	// no rationale enforcement, no surface awareness.
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWith(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	})
	payload := decodeResult(t, res)
	if payload["hit"] != "task_complete" {
		t.Errorf("legacy DispatchWith with empty rationale: handler not reached, payload=%+v", payload)
	}
}

func TestDispatchWithOptions_GateErrorEnvelopeIsStructured(t *testing.T) {
	// Pin the wire-level field set so callers can rely on it.
	policyReg := loadPolicyFromBody(t, `[work.task_complete]
requires_rationale = true
`)
	table := dispatch.Table{"task_complete": stubHandler("task_complete")}
	res, _, _ := dispatch.DispatchWithOptions(agentCtx(), nil, table, dispatch.Args{
		Action: "task_complete",
	}, dispatch.Options{Policy: policyReg, Surface: "work"})
	payload := decodeResult(t, res)
	wantKeys := []string{"error", "action", "surface", "field", "reason", "hint"}
	for _, k := range wantKeys {
		if _, ok := payload[k]; !ok {
			t.Errorf("envelope missing key %q (got %+v)", k, payload)
		}
	}
	hint, _ := payload["hint"].(string)
	if !strings.Contains(strings.ToLower(hint), "rationale") {
		t.Errorf("hint=%q, want it to mention 'rationale'", hint)
	}
}
