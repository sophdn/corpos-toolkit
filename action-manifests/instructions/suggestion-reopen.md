# suggestion-reopen

## When to use

Use when a previously-resolved suggestion needs to come back to open — typically because
the conditions that drove the deferred / rejected decision have changed, or new evidence
warrants revisiting an earlier adopted+routed decision.

## When not to use

- The suggestion is already open — `suggestion_reopen` rejects already-open rows
- The original resolution rationale still holds — leave it resolved
- Reopening a bug — use `bug_reopen` (separate entity)

## What it does

Flips `status` back to `open` and clears:
- `resolution_kind`
- `resolution_note`
- `routed_chain_slug` / `routed_task_slug` / `routed_bug_slug`
- `resolved_at`

Emits a `SuggestionReopened` event carrying a snapshot of the resolution being reversed
(in `previous_resolution`) so the ledger preserves the round-trip without forcing a walk
back through earlier events.

## Parameters

| Parameter | Type | Notes |
|---|---|---|
| `slug` | string | Identifier (slug or numeric id). Aliased as `id`. |

## Envelope-level rationale

Agent actors must supply `rationale` at the envelope level (e.g. "new benchmark evidence
overrides the original deferred decision"). Empty / boilerplate / <6-char rationales are
rejected at the dispatcher.
