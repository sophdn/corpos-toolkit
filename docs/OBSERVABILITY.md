# Structured Observability — toolkit-server

> **Status:** Living spec. Produced by chain `agent-first-substrate` T5 (`structured-observability`). The request-scoped span_id contract documented here is **load-bearing** for the sibling chain `query-telemetry-substrate`; handlers added to `go/internal/knowledge/` or `go/internal/work/` MUST inherit it, never regenerate it.
>
> **Reading order:** §1 framing → §2 ctx contract → §3 the request-scoped rule → §4 emitting spans → §5 SSE wire shape → §6 dashboard surface → §7 lint gate.
>
> **Companion docs:** `docs/EVENT_SUBSTRATE.md` §6 (envelope-level span_id, design); `docs/AGENT_AUDIT_CONVENTIONS.md` §11 (why structured + spans, not flat logs).

---

## 1. What this substrate is

Three streams of observability data share one identity:

1. **Events ledger** (`events` table, T2 of this chain) — every state mutation lands here as an append-only row.
2. **Structured log entries** (`log/slog` JSON via `go/internal/obs/`) — every meaningful operation produces a typed line on stderr.
3. **Grounding events** (`grounding_events` table, originally migration 019; span_id added in 034) — every read-side knowledge retrieval (vault_search / kiwix_search / knowledge_search) records what it returned.

The join key is `span_id`. One MCP request mints exactly one span_id at the dispatch boundary; every event row, every log entry, and every grounding-events row written while serving that request carries the same span_id. A query joining the three on `span_id` reconstructs the full picture of one agent operation.

This is the "logs and events share identity" rule from `AGENT_AUDIT_CONVENTIONS.md` §11, made concrete.

---

## 2. The ctx contract

`go/internal/obs/` owns the `Span` ctx key. The substrate primitives:

| Function | Purpose |
|---|---|
| `obs.SpanStart(ctx, name) (newCtx, end)` | Open a span. Returns a ctx carrying the span and an `end` closure the caller invokes to close the span. |
| `obs.SpanFromContext(ctx) *Span` | Read the active span from ctx. Nil if no span is open. |
| `obs.WithSpan(ctx, span)` | Attach a span to ctx. Also stamps `events.WithSpanID(ctx, span.ID)` so the events package sees the same id without a second ctx mutation. |
| `obs.Logger(ctx) *slog.Logger` | Return the package logger with `span_id`, `parent_span_id`, `trace_id` attrs pulled from ctx. Every log line auto-carries the span context. |
| `obs.L() *slog.Logger` | Bare logger (no span attrs). Used at server init time, before the dispatcher mints a request span. |
| `obs.Fatalf(format, args...)` | Init-time fatal. Writes a structured error entry and exits with code 1. Migration target for the old `log.Fatalf` shape. |

### Field shape on every log entry

```json
{
  "time": "2026-05-17T06:18:07.123Z",
  "level": "INFO",
  "msg": "dispatch.request",
  "span_id": "9f8e7d6c-…",
  "parent_span_id": "",
  "trace_id": "9f8e7d6c-…",
  "surface": "work",
  "action": "task_complete",
  "project": "mcp-servers"
}
```

`parent_span_id` is empty for the root request span; non-empty for child spans opened mid-request. `trace_id` always equals the root span's id, so an `IN` query over all entries in a trace is `WHERE trace_id = ?`.

---

## 3. The request-scoped span_id rule

**This is the load-bearing rule for the sibling chain `query-telemetry-substrate`. Read carefully before adding a handler under `go/internal/knowledge/` or `go/internal/work/`.**

> The dispatcher mints exactly one `span_id` per MCP `tools/call` request. That span_id is stamped onto ctx via `obs.SpanStart` before any handler runs. Every downstream handler MUST inherit it from ctx — handlers MUST NOT call `events.WithSpanID(ctx, freshUUID)`, MUST NOT call `obs.WithSpan(ctx, &Span{ID: freshUUID})`, MUST NOT regenerate a span mid-request.

### Why this matters

The sibling chain's read-write join (`query_resolutions` table, TT2) groups one agent operation's read-side calls and write-side mutations by shared span_id. Concretely:

1. Agent does `vault_search(...)` — read-side; grounding_events row written with span_id S.
2. Agent reads results, decides to act.
3. Agent does `task_complete(...)` — write-side; events row written with span_id S.

The query `SELECT … FROM grounding_events g JOIN events e ON g.span_id = e.span_id` reconstructs the (read → resolution) pairing. If `task_complete`'s handler regenerates a span_id mid-request, the join silently produces orphan rows and the sibling chain's training-data extraction (TT3) breaks.

**Note on agent-turn vs request scope.** The current design mints a span per MCP `tools/call`, not per agent turn. Two `tools/call` requests in the same agent turn (the vault_search followed by task_complete above) get DIFFERENT span_ids in production today; the in-process integration test in `go/internal/obs/integration_test.go` exercises the *handler-level* contract under a shared ctx, which is what handlers can be tested against deterministically. Agent-turn-level correlation (one ID across multiple tools/call from the same LLM completion) is an open design question, flagged in `EVENT_SUBSTRATE.md` §9 Q6.

### Concrete dos and don'ts

| Pattern | Allowed? |
|---|---|
| Open a child span inside a handler: `ctx, end := obs.SpanStart(ctx, "forge.index_upsert")` | ✓ Child inherits parent's trace_id, mints a fresh span_id. Events emitted under the child carry the CHILD span_id. |
| Read span via `obs.SpanFromContext(ctx)` to log a structured field | ✓ |
| Call `events.WithSpanID(ctx, "deadbeef-…")` in a handler | ✗ Regeneration. Use `obs.SpanStart` if you want a child; otherwise leave span_id alone. |
| Detach span from ctx (`obs.WithSpan(ctx, nil)`) before downstream call | ✗ Breaks the join. |
| Spawn a goroutine for in-tx work without threading ctx | ✗ The goroutine's `events.Emit` lands with an auto-generated span_id, orphaning it from the request. Thread ctx through. |
| Add a new mutating action handler: call `events.Emit(ctx, tx, ...)` | ✓ Emit reads span_id from ctx. No special handling needed. |

---

## 4. Emitting spans in handlers

The dispatcher (`go/internal/dispatch/dispatch.go`) opens the **root** span automatically. Handler authors should open **child** spans for distinct sub-operations whose timing/error rate matters to operations debugging:

```go
func HandleSomething(ctx context.Context, deps Deps, params json.RawMessage) (Result, error) {
    // The dispatcher already opened a root span named "work.something"
    // (or similar). No need to repeat it at the top of the handler.

    // Open a child span for the expensive sub-operation.
    ctx, end := obs.SpanStart(ctx, "something.qwen_dispatch")
    defer end(nil) // pass non-nil on error path; see below.

    obs.Logger(ctx).Info("calling qwen", slog.String("model", deps.ModelName))

    result, err := deps.Router.Generate(ctx, prompt, system)
    if err != nil {
        end(err) // record error status
        return Result{}, err
    }

    obs.Logger(ctx).Info("qwen response", slog.Int("tokens", result.Tokens))
    return Result{Text: result.Text}, nil
}
```

### When to open a child span

- Distinct unit of work whose latency or failure rate is interesting on its own (FTS5 index sync, Qwen dispatch, in-transaction projection refresh).
- Operations that span a goroutine boundary (without a span, the goroutine's emits orphan from the request).
- Anything you'd want to render as its own row in the dashboard's span tree.

### When NOT to open a child span

- Pure CPU work with no I/O.
- A loop that fires many short ops (one span per loop iteration would flood the bus); aggregate metrics suffice.

---

## 5. SSE wire shape: `/events/spans`

The toolkit-server's observe-HTTP router mounts `/events/spans` when `--http-port` is set. Each span open and close fans out as one JSON object:

```json
// span_open
{
  "type": "span_open",
  "span_id": "9f8e7d6c-…",
  "parent_span_id": "",
  "trace_id": "9f8e7d6c-…",
  "name": "work.task_complete",
  "started_at": "2026-05-17T06:18:07.123Z"
}

// span_close (immediately after the handler returns)
{
  "type": "span_close",
  "span_id": "9f8e7d6c-…",
  "parent_span_id": "",
  "trace_id": "9f8e7d6c-…",
  "name": "work.task_complete",
  "started_at": "2026-05-17T06:18:07.123Z",
  "duration_ms": 27,
  "status": "ok"
}
```

`status` is `"ok"` on a nil error to `end`, `"error"` otherwise. On error, an additional `error` field carries the message. Dashboard consumers MUST handle both types.

The keep-alive shape matches `/events`: one `: keep-alive\n\n` line every 30 s (configurable in `obs.SpanBus.HandlerWithKeepAlive`).

### Buffer and drop semantics

`SpanBus.Publish` is non-blocking. A subscriber whose 64-event buffer fills will silently drop subsequent events for that subscriber — same behaviour as `eventbus.Bus`. Consumers refresh from a recent-history query on drop; today the SpansPanel just shows what it has captured since mount.

---

## 6. Dashboard surface

The SpansPanel (`apps/dashboard/src/pages/Spans/`) subscribes to `/events/spans`, folds events into per-trace state, and renders a tree:

- Top-level rows are root spans (one per MCP request), newest first.
- Expanding a row shows its child spans sorted by `started_at`.
- Status badges distinguish open / ok / error spans.
- The panel is intentionally bare-minimum; UX polish (filtering, search, drill-into-events) is deferred to a follow-on chain.

---

## 7. Lint gate

`scripts/precommit.sh` §0d2 (added in T5) scans the agent-primary Go paths for `log.Printf` / `log.Fatalf` / `fmt.Println`:

```text
go/internal/work/
go/internal/forge/
go/internal/knowledge/
go/internal/measure/
go/internal/admin/
```

Any match fails the precommit gate with a path:line list. CLI scaffolding under `go/cmd/toolkit-server/` is out of scope (those are user-facing stdout, not structured logs). Test files are excluded by name.

The gate is regression-tested by `scripts/test-precommit-log-gate.sh`, which plants an offending file and asserts the scan picks it up.

---

## 8. Migration story for new packages

If you're adding a new package under `go/internal/` that emits any log line: use `obs.Logger(ctx).<Level>(...)` from day one. Don't import `log/slog` directly — `obs.Logger` returns a `*slog.Logger`, so the call sites look the same, but you get span context for free and the lint gate stays satisfied if your package later lands under an agent-primary path.

If you're adding a new MCP action: emit through `events.Emit(ctx, tx, …)` with a typed payload. The events package reads span_id from ctx — no special handling.

If you're adding an in-transaction hook (a `forge.AfterCreateNotifier`, a fold function, a write-time index sync): open a child span at the top, `defer end(err)` at the close. The hook's emits land under the child span; the dashboard's tree groups them under the request.

---

## 9. Cross-references

- `go/internal/obs/` — package implementation.
- `go/internal/obs/integration_test.go` — regression gate for the span_id propagation rule.
- `docs/EVENT_SUBSTRATE.md` §6 — envelope-level span_id contract.
- `docs/AGENT_AUDIT_CONVENTIONS.md` §11 — why structured + spans.
- `scripts/precommit.sh` §0d2 — lint gate source.
- `scripts/test-precommit-log-gate.sh` — lint gate regression test.
- Sibling chain `query-telemetry-substrate` — read-side observability; depends on the request-scoped rule above.
