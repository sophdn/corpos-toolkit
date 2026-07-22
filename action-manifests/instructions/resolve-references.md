# resolve-references

Unified reference-resolution surface: scan a user message, detect every reference-shape token, dispatch to the appropriate resolver per shape, return ranked binding candidates with tier-classified confidence.

## When to use

Call `knowledge(action='resolve_references', params={message_text: "..."})` when the user message contains specific tokens you can't bind from current context, AND:

- The message contains **multiple** reference-shape tokens. One per-shape tool call (`chain_find`, `task_search`, `vault_search`) is cheaper for one token; this action's value is when you'd otherwise be making three or four targeted calls.
- You want the **unified dispatch** path that also emits resolution telemetry (`grounding_events.query_source='reference_resolution'`, once T5 lands).
- You're not sure which shape the token is — slug? path? skill name? — and want the detector to figure it out from the catalogs.

## When not to use

- **Single known slug.** `work(action='chain_find', params={pattern: 'X'})` is cheaper and the response is closer to the work the agent will do next.
- **Already-bound references.** If the chain / task / bug is already in current context from earlier in the session, don't re-resolve. The action returns the binding fresh from the DB, but you don't need to surface it again.
- **Trivial messages.** "yes", "go on", "thanks", "make the change" — no references. The action returns an empty References list and costs a round-trip.
- **Inline-bound references.** "Work on `ableton-wine-setup` — that's the chain for getting Ableton running under Wine." The user bound it for you; trust the inline binding.
- **Auto-firing on every message.** This action is agent-invoked per the discipline in `skill:reference-resolution`, not system-fired. Auto-firing is the territory of the deferred proactive-injection feature.

## Call shape

```
knowledge(action='resolve_references', params={
  message_text: "<the user message verbatim>",
  top_k_per_shape: 5,        // optional; default 5
  include_no_hits: false,    // optional; default false
  total_budget_ms: 2000,     // optional; default 2000 (the action's internal cap)
})
```

## Response shape

```json
{
  "references": [
    {
      "token": "ableton-wine-setup",
      "shape": "chain_slug",
      "confidence_tier": "single_exact",
      "presented_as": "`ableton-wine-setup` → chain in mcp-servers. status=open tasks=6 pending=4 blocked=2",
      "top_candidates": [{
        "id": "ableton-wine-setup",
        "title": "chain ableton-wine-setup in mcp-servers",
        "score": 1.0,
        "source_ref": "chain:ableton-wine-setup",
        "debug_notes": "status=open tasks=6 pending=4 blocked=2"
      }],
      "recommended_action": "use_directly"
    }
  ],
  "resolution_time_ms": 1062,
  "resolver_calls_made": 3,
  "no_hit_tokens": [],
  "partial_failures": [],
  "truncated_by_budget": false
}
```

## Confidence tiers

| Tier | When | Recommended action |
|---|---|---|
| `single_exact` | 1 hit, score ≥ 0.95 (or shape-specific threshold) | `use_directly` — name what you found, proceed |
| `fuzzy_multi` | 2–5 hits, OR 1 hit with score < 0.95 | `ask_user_to_disambiguate` — list candidates, ask |
| `weak_domain` | Domain-term or external-technical with score < threshold | `mention_as_possibly_relevant` — surface as maybe |
| `no_hit` | 0 candidates | `acknowledge_no_hit_and_ask` — say so, ask user |

## Presentation discipline

When you call `resolve_references`, **name what you found** in your response. Use the `presented_as` strings verbatim or paraphrased. Don't silently incorporate the bindings — that's the proactive-injection territory, which is out of scope for this action.

The three reasons:

1. **Disambiguation** — if you guessed wrong, the user corrects before you start work in the wrong direction.
2. **Debuggability** — the user sees what context shaped the response.
3. **Confidence calibration** — "found X" vs "may refer to Y, score low" tells the user how confident you are.

## Latency

Total budget caps at 2 seconds by default. Tune via `total_budget_ms` if you need to limit further. Rule-based detection runs in ~5ms; domain-term rubric takes ~500ms; kiwix lookup can be ~1.5s. The action returns partial results with `truncated_by_budget=true` rather than waiting indefinitely.

## Cross-references

- The discipline this action implements: `skill:reference-resolution` (`skills/reference-resolution.md`).
- The full design: `docs/REFERENCE_RESOLUTION.md`.
- Per-shape direct alternatives: `work(action='chain_find')`, `work(action='task_search')`, `work(action='bug_list')`, `knowledge(action='vault_search')`, `knowledge(action='kiwix_search')`, `knowledge(action='library_find')`, `knowledge(action='knowledge_search')`.
