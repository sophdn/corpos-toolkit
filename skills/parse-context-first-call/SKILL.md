---
name: parse-context-first-call
description: "Reflex to call `mcp__toolkit-server__knowledge(action='parse_context', params={message_text: <verbatim user message>})` as your FIRST action on every user prompt. The substrate scans the message for every reference shape — chains, tasks, bugs, paths, skills, memory entries, vault notes, kiwix hits, library entries, friction shapes, and trigger-activated disciplines — and returns a unified envelope of Candidates. Acting on the envelope is the orienting move; per-shape direct calls (chain_find, bug_read, vault_search) become the second move."
triggers:
  - parse_context
  - parse-context
  - orient
  - orienting call
  - first call
  - first move
  - librarian computer
  - what's the context
  - what context
---

# Parse-Context-First-Call Discipline

The single bootstrap-load-bearing skill that holds the
librarian-using-the-computer pattern. Without this discipline, the
substrate's value (telemetry feeding the future reranker, friction-shape
detection, cross-substrate context, lazy-loaded skill bodies) does not
accrue — the agent falls back to per-shape direct calls and the
substrate gathers dust.

## Fire rule

Your first action on every user prompt is:

```
mcp__toolkit-server__knowledge(
  action='parse_context',
  params={ message_text: "<the user's verbatim message>" }
)
```

Read the response before reaching for any other surface. The
`references` array surfaces every reference shape detected in the
message, with confidence tiers, recommended actions, and ready-to-quote
`presented_as` strings. Act on whichever are relevant — open the chain
the user named, pull the skill whose trigger fired, surface the vault
note adjacent to the domain term — before continuing with task-specific
work.

`resolve_references` is a soft alias of `parse_context`; both dispatch
the same handler core. Prefer `parse_context` as the canonical name.

**Error-path fallback.** If `parse_context` errors at the MCP layer
(`action_not_implemented`, `registry_not_configured`, dispatcher panic),
retry the call as `mcp__toolkit-server__knowledge(action='resolve_references', ...)`
with identical params before falling back to per-shape direct calls. A
registry-configured server returns the same envelope under either name.
The `action_not_implemented` case usually means the stdio binary is older
than the substrate ship — `/mcp reconnect` is the cleanest fix, but the
alias unblocks the orienting call in the meantime.

## Inline skill bodies on use_directly

For `recommended_action=use_directly` references whose shape is
`skill_trigger` or `discipline_skill`, the envelope now carries the skill
body directly. The `body_inlined` field holds the full or truncated body;
`body_truncated` flags whether it was shortened to fit the envelope
budget; `body_summary` holds a 500-byte head when the body was too large
to inline at all. **Read the inlined body and act on it. Do NOT issue a
separate `Read` of the path in `presented_as` — the body is in your
context already.**

The single-most-frequent failure mode this prevents (bug 1429, third
observation): an exemplar from earlier in conversation context becomes
the action heuristic, and the use_directly recommendation never produces
an actual body load. With the body inlined, the exemplar can't beat the
fresh canonical body — they arrive together.

When `body_inlined` is set: act on it directly. When only `body_summary`
is set (pointer-only tier; body > 8 KB): use the summary as the cue and
issue a `Read` for the full body if you need the load-bearing sections
(Fire rule, Skip rule, How to apply, etc.). When neither is set: the
feature flag is off — fall back to `Read` against the path in
`presented_as`.

**Rollout status.** Inlining is gated on the
`TOOLKIT_PARSE_CONTEXT_INLINE_BODIES` env var, default off. While the
flag is off, the envelope shape is byte-identical to the pre-chain-602
output and the manual-fallback (Read the body) applies. Once the flag
flips on server-wide, the inlined body becomes the canonical access path.

Cross-references the procedural-cue memory
`feedback_proactive_friction_vault_filing.md` (the load-body-on-use_directly
cue is one of the three procedural cues that bug 1429 named).

## Skip rule

Skip `parse_context` ONLY when the user message is clearly a
continuation that won't surface new content:

- short conversational acknowledgments: "thanks", "yes", "go ahead", "ok"
- direct slash-commands: `/init`, `/role-X`, `/help`
- continuation references to the immediately-prior assistant turn:
  "apply that fix", "the second one", "do it"
- sub-50-char messages with no nouns

When in doubt, **FIRE**. The substrate-side filter cache (keyed by
`(session_id, token, shape)`) catches redundant calls cheaply — the
same token resolved twice in one session is one fresh resolution plus
one cache-hit, not two fresh resolutions.

## Calibration warning

**Bias toward calling.** False-positive call is cheap (cache dedupes
per-session repeated tokens; latency budget is 4s ceiling but typical
calls land sub-200ms). False-negative skip is expensive — agent doesn't
orient, substrate-mediated discovery doesn't happen, the
librarian-falls-back-to-card-catalog pattern returns.

If you notice yourself reaching for `chain_find` / `task_search` /
`bug_read` / `vault_search` / `kiwix_search` / `admin.action_describe`
/ `Read` as your **first** call on a fresh work-thread, pause:
`parse_context` should have been first. The per-shape calls are for
action operations on already-resolved bindings, not for orientation.

## Empty envelope on broad-orientation messages

`parse_context`'s reference detectors are catalog-keyed and shape-keyed:
slugs (chain / task / bug), paths, skill names, tool names, forge
schemas, library entries, friction shapes, domain terms, memory entry
names. They DO NOT match concept-words (`ml`, `harness`,
`atomic-tasks`, `orchestrator`, `local LLM`) — those are common nouns
the detector has no catalog hit for, so the envelope comes back with
`references: []` and `resolver_calls_made: 0`.

When the user's opening message is broad orientation — "we're going
to swap orchestrators today", "get familiar with the ML side", "ramp
on how the harness ties together" — the parse_context call lands
fast (~10ms) and useful for telemetry but won't actually surface
anything to act on. The first call is still correct (the discipline
is unconditional); the orienting WORK is a different action shape:

- **Memory read** (`/home/user/.claude/projects/-home-sophi-dev/memory/MEMORY.md` → <!-- pii-allow: private-repo infra path, scrubbed on publish per .publish-scrub-map -->
  the index, then targeted `Read` on the entries it points at).
- **Vault search** via `mcp__toolkit-server__knowledge` action
  `vault_search` against the concept terms — the vault carries
  cross-project framing that the catalog-keyed resolvers don't.
- **Recent activity** via `mcp__toolkit-server__work` actions
  `chain_state` / `roadmap_list` / `bug_list` to see what the user is
  most likely talking about.

The parse_context envelope's `resolution_time_ms` + `resolver_calls_made`
fields are the diagnostic: a sub-50ms response with 0 resolver calls
means the message had no slug-shaped or path-shaped content. Don't
re-fire parse_context expecting a different answer; reach for the
orientation actions above.

Closes bug `parse-context-coverage-gap-on-broad-orientation-messages` —
the empty envelope on broad-orientation messages is by design (the
substrate's binding promise is for shape-detectable references), and
the discipline now names the alternative orienting move so future
agents don't conclude `parse_context` "didn't work" and skip the call
on subsequent prompts.

## Failure mode: multi-turn drift on prompts 2+

The fire rule is **every user prompt**, not "first prompt of the
session." Bug 1445: in one observed session the reflex fired on prompt 1
and was silently skipped on prompts 2 and 3 (both contained no obvious
reference shapes and "felt" routine), even though the discipline is
explicit about being unconditional. There's no harness-side enforcement
— the discipline lives in the agent, and conditional firing recreates
the per-prompt decision the skill was designed to eliminate.

The empty-envelope cost is ~10ms. The drift cost is that one of those
"feels routine" prompts will, eventually, contain a reference the agent
misses because it didn't ask.

**Recovery cue.** If you find yourself N turns into a session without
a recent `parse_context` call, fire one for the previous user prompt's
verbatim text retroactively before continuing. Cheaper than retro-
discovering what you missed.

The discipline against this anti-pattern is documented in the auto-
memory entry `feedback_resolve_references_as_orienting_call.md` —
this skill is the codification of that memory.

## Why this skill exists

Authored by chain `reference-resolution-migration` (id 598). Origin:
the librarian-vs-computer framing — `parse_context` is the computer
the substrate trilogy built to save the agent time; without this
discipline as the orienting call, the agent stays in card-catalog mode
and the computer gathers dust.

This skill is one of the few KEEP-AMBIENT survivors after T7's
skill-body paring; every other skill ships as condense-lazy (surfaced
via parse_context's discipline-skill resolver) or pure-lazy (surfaced
via parse_context's skill-trigger resolver). The first-call discipline
HAS to stay ambient because it's the gate that makes the lazy-loading
substrate work at all.
