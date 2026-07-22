---
name: vault-pull-discipline
description: "Reflex to consult ~/.claude/vault/ before starting a task in a domain you've worked in before. Use mcp__toolkit-server__knowledge.vault_search to rank prior decisions/learnings/reference notes by semantic match (local Qwen2.5-32B reranker, ~1s typical), then mcp__toolkit-server__knowledge.vault_read to fetch the picked path. Cross-project: the vault is shared across seed-packet, dm-toolkit, mcp-servers, and any other agent project."
triggers:
  - vault search
  - vault_search
  - prior decision
  - have we decided
  - did we already
  - previous learning
  - previous decision
  - rag-architecture
  - reference doc
  - I've worked on this before
  - what does the vault say
  - decisions/
  - learnings/
  - reference/
---

# Vault Pull Discipline

Before committing to an approach in a domain you've worked in before,
**check the vault first**. The vault at `~/.claude/vault/` holds
durable cross-session knowledge: architectural decisions, dated
learnings, reference docs. Without a pull discipline, agents redo
work the vault already documents, contradict prior decisions, or
surface bugs the vault already explains.

Cross-project: works the same in seed-packet, dm-toolkit, mcp-servers,
or any other project that mounts toolkit-server.

## When to call vault_search

Call **before** committing to an approach in any of these shapes:

- **Domain match.** The task overlaps a domain that has prior vault
  entries:
  - Forge / dispatch / meta-tool ergonomics → `decisions/` and
    `learnings/mcp-servers/` likely have priors.
  - Qwen / llama-server / local LLM config → `learnings/general/` or
    `learnings/mcp-servers/`.
  - Designing retrieval / RAG / embedding layer → `reference/` has
    the architecture doc.
  - Long-form design doc, schema-design note, post-investigation
    writeup → check `decisions/` for an existing one.
- **"Have we decided this?" shape.** Any moment where you'd ask
  "what's our convention for X?" — there's a non-trivial chance the
  convention is in the vault already.
- **"I think I remember…" shape.** Memory is unreliable across
  sessions. The vault is the durable record.

## When NOT to call vault_search

- Trivial tasks that don't touch a known domain (typo fix, mechanical
  rename, formatting).
- The user already gave the convention inline.
- Inside a tight loop where you've just called it and have results
  in context.

## Mechanical call shape

Two MCP actions on the `mcp__toolkit-server__knowledge` meta-tool:

```
mcp__toolkit-server__knowledge(
    action="vault_search",
    params={"query": "<the task in one sentence>", "top_k": 5}
)
```

The query should read like a task description — full sentence, not
keywords — because the local Qwen2.5-32B reranker scores by semantic
match against path+title pairs over the full vault list.

Then for any path that looks relevant:

```
mcp__toolkit-server__knowledge(
    action="vault_read",
    params={"path": "decisions/2026-05-05_toolkit-server-canonical-forge.md"}
)
```

Returns `{path, frontmatter, content, edit_hint}` with frontmatter parsed.

`vault_search` is project-scope-agnostic — vault is cross-project.
Don't pass `project`.

### Editing a vault note after vault_read

The `vault_read` response includes an `edit_hint` naming the absolute
on-disk path. The agent harness's Edit tool requires a Read-tool call
on a file before Edit will accept it — and the harness only tracks
Read-tool calls, not `vault_read`. So even though the note body is
already in your context after `vault_read`, you must:

1. Call `Read` on the absolute path from `edit_hint`.
2. Then call `Edit`.

This is one extra round-trip. The hint exists so you don't burn an
additional one discovering the harness behavior on first hit.

## Trust the response

The reranker is **honest about no-match**: when no vault note fits
the query, `vault_search` returns an empty `results` array (Qwen
explicitly says "no match" rather than hallucinating). An empty
array means the vault doesn't have prior content — proceed without
further pull.

### Truncation signal (vault > 75 notes)

When the vault grows past `MaxVaultCandidates` (75), the response
carries three structured fields the caller should read:

- `candidates_truncated: true`
- `candidates_used: 75` — the number of notes that reached Qwen rerank
- `truncated_note_count: <N>` — how many were excluded by the
  keyword prefilter
- `candidates_hint: "<prose explanation>"`

This is **not** a recency cap — the prefilter scores by weighted
keyword overlap (path + title + tags + summary + body), so older
notes whose body matches the query DO surface in pass-1. The
truncation only excludes notes that scored low on the query.

If the results look stale or off-target when `candidates_truncated`
is set, **rephrase the query with different keywords** — using
terms from the note title or body you suspect exists. The default
reflex of "narrow the query" applies; widening the search by adding
synonymous terms also works since each adds tokens for the
prefilter to match.

The hint string carries the same advice as prose so a human reader
of the response can act on it without needing this skill loaded.

If query-rephrasing fails to surface the right note and the vault
has clearly outgrown the cap (~150+ notes is the typical inflection),
raise the runtime cap via the `TOOLKIT_VAULT_MAX_CANDIDATES` env var
on the toolkit-server's launch.sh (e.g. `TOOLKIT_VAULT_MAX_CANDIDATES=150
./go/launch.sh`) and restart the daemon. Every additional candidate
adds ~100 input tokens to the Qwen 8192-token budget, so don't raise
indefinitely — but the const default of 75 is a starting point, not
a ceiling.

## After acting

When your work changes a prior decision or adds new context, write
back to the vault per `vault-filing-discipline` (the write-side
reflex — cross-project test, subdir routing, frontmatter, pre-send
ritual). Quick subdir reference:
- New cross-project decision → `decisions/<YYYY-MM-DD>_<slug>.md`
- Project-specific dated learning → `learnings/<project>/<YYYY-MM-DD>_<slug>.md`
- Durable reference an agent will reread → `reference/<slug>.md`

Closing the loop keeps the vault accurate so the next session's
agent benefits.

## Latency expectation

Single-pass `vault_search` is ~600ms-1.5s on a 65-note vault running
through Qwen2.5-32B locally. Cheap enough to call ambiently at task
start. If you observe consistent >1.5s p95 latencies, that's the
stage-2 trigger condition for swapping to embedding-based retrieval —
file it as a follow-up rather than tolerating the slowdown.

## Failure modes

Concrete examples of how this discipline gets skipped in practice. Each
is a real session where an agent should have vault-searched first but
didn't — naming the pattern so future agents recognise the shape.

**"Chain design_decisions felt like enough context."** Session 2026-05-15,
chain `go-typed-returns-rollout` T2 pickup. The chain's `design_decisions`
field already named the pattern (typed handler returns, `dispatch.Adapt`,
omitempty for multi-shape responses). The agent treated that summary as
complete context, audited the target file, started designing the typed
result structs, and was halfway through writing `types.go` before the
user reminded "we have notes in the vault around this." vault_search at
that moment surfaced `reference/2026-05-15_go-mcp-dispatch-typed-returns-pattern.md`
— canonical to the task, naming exactly the patterns that matter
(`MarshalJSON` for mode-discrimination, the forbidden `type X = any`
aliasing, the 30-second corpus survey before declaring something
schemaless). The chain field is curated; the vault is the deeper bench.
**Both** get consulted on task start, not one or the other.

Signal that you're at risk of this failure mode: you're about to write
NEW code in a package you've worked in this session, your task spec
names a pattern by reference (e.g. "the established pattern", "see
vault.Frontmatter precedent"), and you haven't called `vault_search` in
this session. Pause; search.

## What this skill is not

- Not a substitute for the user's stated intent. If the user says
  "ignore the vault" or "redo this from scratch", honour that — this
  is a default reflex, not a forcing function.
- Not a write surface. `vault_read` is read-only.
- Not a full RAG with embeddings — currently Qwen ranks against the
  full path list. Stage-2 (fastembed + sqlite-vec) triggers when
  vault > 150 notes OR p95 > 1500ms.
