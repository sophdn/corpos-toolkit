package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolkit/internal/dispatch/policy"
	"toolkit/internal/events"
	"toolkit/internal/obs"
)

// IntrospectAction is the pseudo-action all four meta-tools recognise to
// return their supported-action list. The double-underscore prefix keeps
// it out of the schema namespace — no real action uses this name.
const IntrospectAction = "__actions__"

// Args is the shared argument struct for all 4 meta-tools. Handlers receive
// Params re-encoded as json.RawMessage via Dispatch. Project carries the
// optional top-level project scope shared across all per-action dispatches
// (matches Rust toolkit-server's top-level project field).
//
// Note: this struct's auto-inferred JSON Schema would emit "params": true
// (a JSON Schema 2020-12 boolean schema) for the any-typed Params field,
// which the Claude Code harness's Zod validator rejects — silently dropping
// every tool. Tool registrations in cmd/toolkit-server set an explicit
// InputSchema via [MetaToolInputSchema] to avoid that.
type Args struct {
	Action  string `json:"action"`
	Params  any    `json:"params,omitempty"`
	Project string `json:"project,omitempty"`
	// Cwd is the caller's current working directory. When set and Project is
	// empty, the server resolves the effective project by matching Cwd against
	// admin.project_list paths (see [ProjectResolver]). Optional — callers
	// that don't pass it fall back to the configured server-wide default.
	Cwd string `json:"cwd,omitempty"`
	// Rationale is the per-call "why" string an agent actor supplies on
	// every mutating action. Enforcement lives in [DispatchWith] via the
	// policy registry; handlers see this value via [events.RationaleFromContext]
	// after the dispatcher stamps it onto ctx. Per the T3 design
	// (docs/EVENT_SUBSTRATE.md §5), rationale is envelope-level, not
	// payload-level — it sits next to action/project, not inside params.
	Rationale string `json:"rationale,omitempty"`
}

// MetaToolInputSchema returns the JSON Schema all four meta-tools (work,
// measure, knowledge, admin) share. Params is typed as a plain object;
// omitting it is already permitted because the `required` list contains
// only "action", and the `json:"params,omitempty"` struct tag handles
// absent-value serialisation. A JSON-Schema type-array like
// ["object", "null"] is rejected by the Claude Code Zod validator,
// which silently drops every tool whose inputSchema contains one.
func MetaToolInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":    map[string]any{"type": "string"},
			"params":    map[string]any{"type": "object", "additionalProperties": true},
			"project":   map[string]any{"type": "string"},
			"cwd":       map[string]any{"type": "string"},
			"rationale": map[string]any{"type": "string"},
		},
		"required":             []any{"action"},
		"additionalProperties": false,
	}
}

// Handler processes a single named action. The project string is the per-call
// project scope; handlers that don't need it ignore the argument.
//
// The `any` return is load-bearing at this seam: it is the JSON-marshaling
// boundary for every action. Concrete handlers should return named result
// structs via [Adapt] rather than `any` directly.
type Handler func(ctx context.Context, project string, params json.RawMessage) (any, error)

// Table maps action names to handlers for one meta-tool.
type Table map[string]Handler

// Adapt converts a typed-return handler into the Handler signature the
// dispatch table expects. Every meta-tool action handler should return a
// concrete result type (named struct, []Row, etc.); Adapt is the single
// place where that concrete type widens to `any` for JSON marshaling.
//
// This is the only legitimate widening of a typed handler return to `any`
// in the codebase. The forbidigo lint rule treats `any` everywhere else as
// an error.
func Adapt[T any](h func(context.Context, string, json.RawMessage) (T, error)) Handler {
	return func(ctx context.Context, project string, params json.RawMessage) (any, error) {
		return h(ctx, project, params)
	}
}

// AdaptNoParams is the same as Adapt for handlers that ignore the project
// and params arguments — convenience for the common "(ctx) → T" shape
// without forcing every caller to write a discarding closure.
func AdaptNoParams[T any](h func(context.Context) (T, error)) Handler {
	return func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		return h(ctx)
	}
}

// AdaptParamsOnly is for handlers that need params but not project.
func AdaptParamsOnly[T any](h func(context.Context, json.RawMessage) (T, error)) Handler {
	return func(ctx context.Context, _ string, params json.RawMessage) (any, error) {
		return h(ctx, params)
	}
}

// AdaptWithDeps curries a per-action deps struct (e.g. forge.Deps) into a
// typed-return handler so callers can register the bare function without
// writing an ad-hoc closure. The deps value is captured at registration
// time and threaded into every invocation.
//
// Use this when the handler needs a shared injected dependency that the
// dispatch.Adapt signature (`ctx, project, params`) doesn't carry.
func AdaptWithDeps[D, T any](deps D, h func(context.Context, D, string, json.RawMessage) (T, error)) Handler {
	return func(ctx context.Context, project string, params json.RawMessage) (any, error) {
		return h(ctx, deps, project, params)
	}
}

// ProjectResolver picks the effective project for one Dispatch call.
// Resolution policy is the resolver's responsibility; the dispatcher just
// calls it and threads the result into the handler. Typical resolution
// order is:
//
//  1. args.Project (explicit per-call wins)
//  2. args.Cwd matched against admin.project_list paths (longest-prefix)
//  3. a server-wide --default-project value
//  4. ""  (cross-project; read handlers fall back to no project filter)
type ProjectResolver func(args Args) string

// StaticProjectResolver builds a resolver that ignores Cwd and returns the
// per-call args.Project, falling back to the server-wide default. Useful
// for tests and for servers that don't expose a project registry.
func StaticProjectResolver(defaultProject string) ProjectResolver {
	return func(args Args) string {
		if args.Project != "" {
			return args.Project
		}
		return defaultProject
	}
}

// Options bundles the optional dependencies threaded into [DispatchWith].
// The zero value is usable: no policy enforcement (every action passes),
// no surface name (action-level policy lookups skipped).
//
// Pre-T3 callers using bare [DispatchWith] without an Options got the
// equivalent of an unset Policy + empty Surface; the new [DispatchWith]
// preserves that behaviour via [Options.Zero].
type Options struct {
	// Policy is the dispatch-layer enforcement registry. When non-nil
	// and the gates for (Surface, args.Action) require rationale, the
	// dispatcher rejects empty/boilerplate/short rationale calls from
	// agent actors before invoking the handler.
	Policy *policy.Registry
	// Surface is the meta-tool name (work, knowledge, measure, admin)
	// used to scope policy lookups. Required when Policy is set.
	Surface string
	// CaptureSpanID, when non-nil, receives the request-scoped span_id this
	// dispatch opens (the same id every event / grounding_events row inserted
	// while serving inherits). The HTTP layer uses it to surface span_id on the
	// response so a client can link follow-up telemetry (e.g. query_interactions
	// off a search's grounding_event) back to the call. Nil for callers that
	// don't need it (the default) — zero behavior change.
	CaptureSpanID *string
}

// Dispatch routes args.Action to its handler in table. The defaultProject
// is substituted when args.Project is empty.
//
// Unregistered actions return a structured JSON error payload listing the
// supported actions so callers can recover without out-of-band documentation.
// The pseudo-action [IntrospectAction] short-circuits to that list explicitly.
//
// This is the legacy fixed-default entry point; new call sites should use
// [DispatchWith] to plumb a CWD-aware [ProjectResolver].
func Dispatch(ctx context.Context, defaultProject string, table Table, args Args) (*mcp.CallToolResult, any, error) {
	return DispatchWith(ctx, StaticProjectResolver(defaultProject), table, args)
}

// DispatchWith is Dispatch with a pluggable [ProjectResolver] in place of a
// bare defaultProject string. The resolver receives the full Args (so it can
// see Cwd as well as Project) and returns the project that gets threaded
// into the handler. Policy enforcement is off in this form — use
// [DispatchWithOptions] for the full T3 surface.
func DispatchWith(ctx context.Context, resolver ProjectResolver, table Table, args Args) (*mcp.CallToolResult, any, error) {
	return DispatchWithOptions(ctx, resolver, table, args, Options{})
}

// DispatchWithOptions is [DispatchWith] with explicit policy + surface
// plumbing. The four MCP meta-tool wires in cmd/toolkit-server pass their
// surface name plus the loaded *policy.Registry so the dispatcher can
// reject agent-actor calls that don't carry rationale on mutating actions.
//
// On rationale-gate failure the response is a structured JSON envelope
// with `error: "rationale_required"`, the InvalidInput field name
// (`field: "rationale"`), the agent-readable reason, and a hint. The
// handler is NOT invoked.
//
// Observability: this entry point opens a top-level [obs.Span] named
// `<Surface>.<Action>` (or `dispatch.<Action>` when no Surface is set)
// and threads the resulting ctx through every handler. Every event
// emitted, every log line written, and every grounding-events row
// inserted while serving the request inherits the same span_id from
// ctx — see docs/OBSERVABILITY.md §"Request-scoped span_id". Handlers
// MUST NOT regenerate a span_id mid-request; child operations open
// nested spans via [obs.SpanStart].
func DispatchWithOptions(ctx context.Context, resolver ProjectResolver, table Table, args Args, opts Options) (*mcp.CallToolResult, any, error) {
	// Open the request-scoped root span. The span name encodes the
	// surface + action so a tree-render groups every dispatch under a
	// human-readable label. Defer end with the dispatcher's eventual
	// return error captured by closure — set inside the function body
	// so a panic-recover or normal return both flow through it.
	spanName := args.Action
	if opts.Surface != "" {
		spanName = opts.Surface + "." + args.Action
	}
	ctx, endSpan := obs.SpanStart(ctx, spanName)
	var dispatchErr error
	defer func() { endSpan(dispatchErr) }()

	// Surface the request span_id to a caller that asked for it (the HTTP layer
	// echoes it on the response so clients can link follow-up telemetry). Read
	// from ctx so it matches exactly what events/grounding rows inherit.
	if opts.CaptureSpanID != nil {
		if s := obs.SpanFromContext(ctx); s != nil {
			*opts.CaptureSpanID = s.ID
		}
	}

	// Bug 1070: honor a `project` nested inside params. A caller that writes
	// {action, params:{..., project:"corpos"}} instead of {action,
	// project:"corpos", params:{...}} would otherwise have the nested key
	// silently dropped by every handler's json.Unmarshal (param structs have
	// no Project field), and the empty top-level Project would fall through to
	// the server's --default-project — returning confidently-wrong empties for
	// an intended cross-project / other-project query with NO signal that the
	// requested scope was never applied. We promote the params-nested project
	// into args.Project here, BEFORE resolution, but only when the top-level
	// Project is empty: the envelope field stays canonical and always wins.
	// This mirrors the paramsContainNonEmptyRationale promotion (bug 1403) and
	// covers every action on every surface in one place. Strictly additive —
	// it only fires where a project would previously have been silently
	// dropped, so no legitimate cross-project (empty-everywhere) call changes.
	if args.Project == "" {
		if nested := projectFromParams(args.Params); nested != "" {
			args.Project = nested
		}
	}

	// Thin work-surface call telemetry (chain quiet-and-instrument-operator-
	// surface T1): time the dispatch + classify the outcome and hand it to the
	// decoupled CallObserver (set by the server; nil in tests/bare use, so this
	// is a no-op there). project is resolved up-front so it's available to the
	// deferred record even on the early-return paths (unknown action, rationale
	// reject). errClass is set at each return site; "" means success.
	project := ""
	if resolver != nil {
		project = resolver(args)
	} else if args.Project != "" {
		project = args.Project
	}
	// Read actions are cross-project by default (actiondocs.WorkDescription).
	// The resolver above substitutes the session/CWD/default project whenever
	// no explicit scope was supplied (args.Project == "" after the bug-1070
	// params-nested promotion above). For a READ that is wrong — it silently
	// collapses an unscoped cross-project query to one project. So drop the
	// injected default for known cross-project reads, leaving "" (no project
	// filter). Writes are untouched: IsCrossProjectRead is a fail-safe
	// allowlist, so a mis-classification can never strip a write's default.
	// Bug read-actions-inherit-session-default-project-breaking-cross-project-read-contract.
	if args.Project == "" && IsCrossProjectRead(opts.Surface, args.Action) {
		project = ""
	}
	started := time.Now()
	var errClass string
	defer func() {
		if callObserver != nil {
			callObserver(ctx, opts.Surface, args.Action, project, time.Since(started), errClass)
		}
	}()

	obs.Logger(ctx).Info("dispatch.request",
		slog.String("surface", opts.Surface),
		slog.String("action", args.Action),
		slog.String("project", args.Project),
	)

	if args.Action == IntrospectAction {
		return jsonResult(map[string]any{
			"actions": sortedActionNames(table),
		})
	}

	h, ok := table[args.Action]
	if !ok {
		errClass = "unknown_action"
		obs.Logger(ctx).Warn("dispatch.unknown_action",
			slog.String("action", args.Action),
			slog.String("surface", opts.Surface),
		)
		return jsonResult(map[string]any{
			"error":     "action not implemented",
			"action":    args.Action,
			"supported": sortedActionNames(table),
		})
	}

	// Rationale gate. Runs BEFORE handler dispatch so a reject leaves
	// no DB writes behind. Read-only actions are unaffected: the policy
	// gates for them return RequiresRationale=false (or the policy
	// registry is nil), so ValidateRationale is a no-op short-circuit.
	if opts.Policy != nil && opts.Surface != "" {
		actor := events.ActorFromContext(ctx)
		gates := opts.Policy.Gates(opts.Surface, args.Action)
		if err := gates.ValidateRationale(actor.Kind, args.Rationale); err != nil {
			var re *policy.RationaleError
			if errors.As(err, &re) {
				// Bug 1403: distinguish "agent forgot to write a
				// rationale" from "agent wrote one but nested it inside
				// params instead of at the envelope level". The validator
				// only sees the empty top-level Rationale; the dispatcher
				// can inspect Params to disambiguate before responding.
				if re.Reason == "empty" && paramsContainNonEmptyRationale(args.Params) {
					re = &policy.RationaleError{
						Field:  re.Field,
						Reason: "wrong_nesting",
						Hint:   "rationale was supplied inside `params` but is an envelope-level field. Move it next to action/project: {action: '...', params: {...}, rationale: '...'} — not {action: '...', params: {..., rationale: '...'}}.",
					}
				}
				errClass = "rationale_" + re.Reason
				return jsonResult(map[string]any{
					"error":   "rationale_required",
					"action":  args.Action,
					"surface": opts.Surface,
					"field":   re.Field,
					"reason":  re.Reason,
					"hint":    re.Hint,
				})
			}
			errClass = "rationale_error"
			return jsonResult(map[string]any{
				"error":  err.Error(),
				"action": args.Action,
			})
		}
		// Gate passed (or was off). Stamp the validated rationale onto
		// ctx so downstream events.Emit calls pick it up without each
		// handler re-threading the string. Only when the gate is active
		// AND the actor is an agent do we stamp; for human/system the
		// rationale is recorded verbatim when supplied but isn't
		// required, so we don't want a stale ctx value bleeding into a
		// later cross-actor reuse of the ctx.
		if args.Rationale != "" {
			ctx = events.WithRationale(ctx, args.Rationale)
		}
	}

	var rawParams json.RawMessage
	if args.Params != nil {
		var encErr error
		rawParams, encErr = json.Marshal(args.Params)
		if encErr != nil {
			errClass = "bad_params"
			return jsonResult(map[string]string{
				"error":  "marshal params: " + encErr.Error(),
				"action": args.Action,
			})
		}
	}

	// project resolved up-front (see the telemetry capture above).
	out, err := h(ctx, project, rawParams)
	if err != nil {
		dispatchErr = err
		errClass = "handler_error"
		obs.Logger(ctx).Error("dispatch.handler_error",
			slog.String("action", args.Action),
			slog.String("surface", opts.Surface),
			slog.String("err", err.Error()),
		)
		return jsonResult(map[string]string{
			"error":  err.Error(),
			"action": args.Action,
		})
	}
	errClass = classifyResult(out)
	obs.Logger(ctx).Info("dispatch.response",
		slog.String("surface", opts.Surface),
		slog.String("action", args.Action),
	)
	return jsonResult(out)
}

// CallObserver receives one record per dispatch for the thin work-surface
// call-telemetry spine (chain quiet-and-instrument-operator-surface T1).
// latency is wall-clock for the whole dispatch (gate + handler); errClass is
// "" on success or a short classifier otherwise. The server sets this via
// [SetCallObserver] to a fail-open DB writer; it is nil in tests and bare
// dispatch use, so capture is zero-cost there. Kept as a hook so the dispatch
// package takes no DB/telemetry dependency.
type CallObserver func(ctx context.Context, surface, action, project string, latency time.Duration, errClass string)

var callObserver CallObserver

// SetCallObserver installs the per-dispatch telemetry observer. Last writer
// wins; pass nil to disable. Not concurrency-guarded — call once at startup.
func SetCallObserver(o CallObserver) { callObserver = o }

// classifyResult derives an error_class from a handler's result envelope for
// the success path (err == nil). Many handlers signal rejections in-band via
// a non-empty Error string field (with err == nil) — e.g. forge missing-
// required / unknown-param — so those look like successes unless we inspect
// the envelope. Returns "" for a true success, the envelope's Kind classifier
// when present (e.g. ViolationMissingRequired), else "rejected". Best-effort +
// panic-safe: any reflection surprise yields "" rather than disrupting the
// call. (Result types using a pointer Err envelope rather than an Error string
// fall through as "" — acceptable for the thin spine; the param-fumble signal
// lives in the Error/Kind-string envelopes.)
func classifyResult(out any) (class string) {
	defer func() {
		if recover() != nil {
			class = ""
		}
	}()
	v := reflect.ValueOf(out)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	// Shape 1: a non-empty Error string field (forge / lifecycle envelopes).
	// Prefer the Kind classifier (e.g. ViolationMissingRequired) when present.
	if structStringField(v, "Error") != "" {
		if kind := structStringField(v, "Kind"); kind != "" {
			return kind
		}
		return "rejected"
	}
	// Shape 2: a non-nil pointer Err envelope (e.g. TaskReadResult.Err
	// *ErrorEnvelope) rather than an Error string — the read-handler
	// rejection shape, which carries the id/slug-resolution fumbles.
	if errPtr := v.FieldByName("Err"); errPtr.IsValid() && errPtr.Kind() == reflect.Pointer && !errPtr.IsNil() {
		return "rejected"
	}
	return ""
}

// structStringField reads a named string field from a struct value, or "" if
// absent / not a string.
func structStringField(v reflect.Value, name string) string {
	f := v.FieldByName(name)
	if f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

// sortedActionNames returns the table's action names lexically sorted.
// Derived from the same map the dispatcher uses, so the list cannot drift
// from the registered handlers.
func sortedActionNames(table Table) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// paramsContainNonEmptyRationale checks whether the caller supplied a
// non-empty "rationale" key inside params (a common authoring mistake
// — rationale is an envelope field, not a params field). Returns false
// when params is nil, not object-shaped, missing the key, or carries
// only an empty/whitespace value. Used to upgrade the "empty" rationale
// error into a "wrong_nesting" hint per bug 1403.
func paramsContainNonEmptyRationale(p any) bool {
	if p == nil {
		return false
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	rat, ok := obj["rationale"]
	if !ok {
		return false
	}
	var s string
	if err := json.Unmarshal(rat, &s); err != nil {
		return false
	}
	return strings.TrimSpace(s) != ""
}

// projectFromParams extracts a non-empty "project" value nested inside the
// params object — the common authoring mistake the dispatcher repairs for
// bug 1070 (project is an envelope-level field, not a params field). Returns
// "" when params is nil, not object-shaped, missing the key, or carries only
// an empty/whitespace value. Symmetric with [paramsContainNonEmptyRationale],
// which repairs the same nesting mistake for the rationale envelope field.
func projectFromParams(p any) string {
	if p == nil {
		return ""
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	val, ok := obj["project"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(val, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}
