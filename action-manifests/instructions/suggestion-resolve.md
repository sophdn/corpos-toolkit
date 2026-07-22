# suggestion-resolve

## When to use

Use when a suggestion is ready to leave the open state for one of three reasons:

- **adopted** — the proposal is accepted. May include `routed_chain_slug` + `routed_task_slug`
  naming the chain+task that will (or did) implement it, and `routed_bug_slug` naming a bug
  filed for the implementation. If the implementing commit has landed, pass `commit_sha`
  too (with no `kind`, supplying `commit_sha` defaults `kind=adopted` — the dominant
  'work shipped' shape).
- **deferred** — agreed in principle but not now. `resolution_note` typically carries
  the revisit signal (when the conditions change, reopen and adopt).
- **rejected** — the proposal is declined. `resolution_note` carries the reasoning.

## When not to use

- The suggestion was previously resolved and needs to come back to open — use `suggestion_reopen`
- The implementing commit lands AFTER the resolve — stamp it later with `suggestion_stamp_sha`
- Filing a new suggestion — use `forge(suggestion, …)`
- Resolving a bug — use `bug_resolve` (separate entity, separate vocabulary)

## Vocabulary — distinct from bug_resolve

| suggestion_resolve | bug_resolve |
|---|---|
| adopted | fixed |
| deferred | wontfix |
| rejected | wontfix (different rationale) |
| — | upstream |
| — | dup |
| — | routed (suggestions use `kind=adopted` + `routed_*` for the same shape) |

`fixed`, `wontfix`, `upstream`, `dup`, `routed` are **rejected** with an explicit error
naming the suggestion-side vocabulary. The two surfaces stay distinct on purpose.

## Verb-form aliases

The resolver normalises common verb forms to the past-participle enum value:
- `adopt` → `adopted`
- `defer` → `deferred`
- `reject` → `rejected`

## Parameters

| Parameter | Type | Notes |
|---|---|---|
| `slug` | string | Identifier (slug or numeric id). Aliased as `id`. |
| `resolution_kind` | string | `adopted` / `deferred` / `rejected`. Aliased as `kind`. |
| `commit_sha` | string? | Implementing commit SHA. Aliased as `sha`. Accepts `unversioned`. When supplied, defaults `kind=adopted`. |
| `resolution_note` | string? | Free-text reasoning / what shipped / revisit signal. |
| `routed_chain_slug` | string? | For kind=adopted: the chain absorbing the work. |
| `routed_task_slug` | string? | For kind=adopted: the specific task in routed_chain_slug. |
| `routed_bug_slug` | string? | For kind=adopted: a bug filed for the implementation, if any. |

## Envelope-level rationale

Agent actors **must** supply `rationale` at the envelope level (next to `action` and
`params`, NOT inside params) per `action-manifests/dispatch-policy.toml`. Empty,
whitespace-only, or <6-char rationales are rejected.
