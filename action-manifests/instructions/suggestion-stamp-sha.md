# suggestion-stamp-sha

## When to use

Use when adoption resolved at decision time queued the implementing work in a chain,
and the chain has now landed an implementing commit. Stamps `resolved_commit_sha` on
an already-resolved suggestion without re-running the resolve handler.

## When not to use

- The suggestion is still open — resolve it first via `suggestion_resolve`
  (which accepts `commit_sha` directly: `suggestion_resolve(slug, kind="adopted", commit_sha=…)`)
- The implementing artifact lives outside any git repo — pass `sha="unversioned"` instead
  of a real SHA. The `unversioned` sentinel is accepted by both `suggestion_resolve` and
  `suggestion_stamp_sha`.
- Stamping a bug SHA — use `bug_stamp_sha` (separate entity)

## Parameters

| Parameter | Type | Notes |
|---|---|---|
| `slug` | string | Identifier (slug or numeric id). Aliased as `id`. |
| `commit_sha` | string | The commit that lands the adopted work. Aliased as `sha`. Accepts `unversioned`. |

## Envelope-level rationale

Agent actors must supply `rationale` at the envelope level — typically the commit
message or a one-liner describing what landed. Bypassing the gate fails with
`rationale_required`.
