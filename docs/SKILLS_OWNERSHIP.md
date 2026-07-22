# Skill ownership — the canonical home of every agent skill

Authoritative output of chain
`skills-ownership-split-toolkit-core-canonical-corpos-manages-the-rest` (T1).
One canonical home per skill, so a correction lands in exactly one place and no
consumer silently diverges.

Sophi's governing rule (2026-07-16): **"corpos-toolkit is canonical for its own
core skills; everything else is managed by corpos."** This document
operationalizes that rule so a *new* skill is classifiable without re-asking.

## The decision rule (settles future cases)

Classify a skill by the **strongest** matching clause, top to bottom:

1. **Temporary / experimental / personal** — a skill deliberately not shipped by
   either repo (scaffolding for a time-boxed program, a personal experiment).
   → **overlay-native**: lives only in `~/.claude/skills`, canonical there.
   Giving a temporary skill a canonical repo home is how it becomes permanent.

2. **Requires toolkit-server's own surfaces** — following the skill is
   impossible without operating the substrate: the work/knowledge ledger
   (`forge`; the bug / suggestion / chain / task / roadmap surfaces),
   `vault_search` / `vault_read` / `forge(vault-note)`, `memory`, `parse_context`,
   `knowledge_search`. The skill would be inoperable, or meaningless, with
   toolkit-server removed. → **corpos-toolkit** (`skills/`), canonical; corpos
   mirrors it into its embed (chain 444 T3).

3. **Everything else** — general craft (debugging, refactoring, coding
   philosophy), stack/framework conventions, and harness/agent workflow. These
   work in any repo and do not depend on toolkit-server. → **corpos**
   (`internal/skills/library` for vanilla craft, `userlib` for operator
   stack/voice).

**Repo-owned vs overlay-native** is the load-bearing distinction: a repo-owned
skill (clauses 2–3) is a *downstream copy* in `~/.claude/skills` and editing it
there is a category error (the 2026-07-16 corrections vanished exactly this
way). An overlay-native skill (clause 1) is *canonical* in the overlay and must
be tracked in `~/.claude` git.

**T4 amendment (2026-07-22) — "owned by" and "committed in" are not the same
question.** The clause 3 assignment above says which project *owns* a skill, and
that is unchanged. But ownership does not by itself produce a committed upstream:
corpos **gitignores** `internal/skills/userlib/*/` (only the README is tracked),
so the 18 operator stack skills it owns have no committed copy there. Their
canonical *committed* home is `~/.claude`, and the flow reverses — corpos's
`userlib` holds the installed copy, not the source. Concretely, `~/.claude/skills`
is two halves:

| half | count | canonical | in `~/.claude` git |
|---|---|---|---|
| repo-owned, committed upstream | 24 | `corpos/internal/skills/library/` (7 of them originating in `corpos-toolkit/skills/`) | **gitignored** — installed copies |
| no committed upstream | 19 | `~/.claude/skills/` itself | **tracked** — 18 userlib-owned + `corpos-swap-rehearsal` |

`~/.claude/.gitignore` names all 24 explicitly and `scripts/install-into-claude.sh`
refuses to overwrite anything `~/.claude` tracks, so the halves cannot bleed into
each other silently. The one-line test for which half you are editing: if the
skill shows up in `git -C ~/.claude status`, that is its home.

### The boundary that decides the borderline cases

Clause 2 is about **using a toolkit surface**, not about **the substrate being
the subject or the target**. A methodology that happens to bookend toolkit
actions (open a bug record, stamp a resolution) but whose *craft* is
substrate-agnostic is clause 3, grouped with `systematic-debugging`. Applied
below to `bug-fixing-discipline` and `cannibalize-discipline` — see rationale.

## Sophi's rulings (honored here)

- `scratchpad-discipline` → **corpos**. "corpos stuff, it should stay." Already
  in `internal/skills/library`; no move.
- `corpos-swap-rehearsal` → **overlay-native / temporary**. "swap rehearsal is
  temporary." NOT homed in a canonical repo; retires when chain 270
  `harness-swap-validation` resolves.
- `state-verification-discipline` → **corpos** (2026-07-21). Confirmed against
  T1's recommendation: nothing in it depends on toolkit-server — its ground
  truths are git, the filesystem, a live query. Same bucket as
  `systematic-debugging`, `refactoring-discipline`. The counter (its worked
  examples are toolkit-flavored) was weighed and the dependency argument won.
- `_template` → **corpos** (2026-07-21). Confirmed after bug 1194 (the
  underscore-skip fix) landed, so the decision was made on the merits — home it
  alongside the library it templates — not on "it pollutes prompts." Now inert
  wherever it lives: corpos's loader skips `_`-prefixed names, and Go's `embed`
  naturally excludes an `_`-prefixed dir, so it is a copy-target that never
  ships. corpos-toolkit's `skills/README.md` points at it.

## The table — all 43 skills

### corpos-toolkit — canonical `skills/<name>/SKILL.md` (7)

Requires toolkit-server surfaces (clause 2).

| skill | requires |
|---|---|
| `bug-filing-discipline` | `forge` kind=bug; `knowledge_search` dedupe |
| `suggestion-filing-discipline` | suggestion surface; `knowledge_search` dedupe |
| `vault-filing-discipline` | `forge(vault-note)` → `knowledge_pointers` |
| `vault-pull-discipline` | `vault_search` / `vault_read` |
| `content-routing` | routing map INTO toolkit surfaces (DB / vault / memory / skills) |
| `parse-context-first-call` | the `parse_context` reflex |
| `deterministic-extraction-discipline` | extract a soft-corpus fact into an owned toolkit service + wire `parse_context` |

The first five **reclassify out of** `corpos/internal/skills/library` (they were
shipped there); corpos keeps embedded *mirrors* synced from this tree (T3). The
last two were previously homeless (overlay-only).

### corpos — `internal/skills/library/` (general craft, 17)

General agent craft + methodology; substrate-agnostic (clause 3).

`bug-fixing-discipline`, `cannibalize-discipline`, `codebase-inspection`,
`code-migration-discipline`, `code-standards`, `coding-philosophy`,
`dependency-vetting-discipline`, `ml-opportunity-scan`, `refactoring-discipline`,
`requesting-code-review`, `scratchpad-discipline`, `spike`,
`state-verification-discipline`, `systematic-debugging`, `writing-plans`,
`prose-conventions`, `_template`.

- `prose-conventions` encodes Sophi's human-audience voice; it is corpos-owned —
  library-vs-`userlib` placement is a corpos-internal call (lean `userlib` as
  operator voice).
- `_template` → `internal/skills/library/_template/` (embed- and loader-excluded
  by its leading underscore; a copy-target, not a shipped skill).
- `state-verification-discipline`, `prose-conventions`, `_template` are
  **decided-for-corpos here but not yet physically in corpos** — they are
  overlay-only today. The move lands as part of the overlay resolution (chain
  444 T4) or a corpos-side commit; this table records the destination.

### corpos — `internal/skills/userlib/` (operator stack conventions, 18)

Stack/framework/platform conventions; operator-specific (clause 3).

**Committed in `~/.claude`, not in corpos** (T4). corpos owns these — its
`userlib` tier is what embeds them into this operator's build — but
`.gitignore: /internal/skills/userlib/*/` keeps them out of the shared repo by
design, so corpos holds no committed copy to restore from. `~/.claude/skills/`
is their canonical committed home and they are **tracked** there; corpos's copy
is the installed one. Editing them in `~/.claude` is correct, and the edit must
be committed there.

`expo-conventions`, `github-auth`, `github-code-review`, `github-issues`,
`github-pr-workflow`, `github-repo-management`, `go-conventions`,
`godot-conventions`, `godot-git-hygiene`, `kiwix-local-docs`,
`layout-conventions`, `node-inspect-debugger`, `python-conventions`,
`python-debugpy`, `rust-conventions`, `self-compile-content-sourcing`,
`telemetry-conventions`, `worktree-workflow`.

### overlay-native — `~/.claude/skills/<name>/` (1)

Temporary (clause 1).

| skill | note |
|---|---|
| `corpos-swap-rehearsal` | scaffolding for the swap program; retires with chain 270. Must be **tracked** in `~/.claude` git (T4). |

## Borderline rationale (the judgment calls)

- **`bug-fixing-discipline` → corpos, not corpos-toolkit.** It pairs by name with
  `bug-filing-discipline` (which is toolkit-core), but the *skill* is a 12-step
  root-cause methodology whose canonical reference is a vault note and whose
  steps ("two paths disagreeing = invariant violation", "regression test first")
  are substrate-agnostic. It opens a bug record and stamps a resolution via
  toolkit actions, but those bookend the craft rather than constitute it — clause
  3's boundary. Grouped with `systematic-debugging`.
- **`cannibalize-discipline` → corpos, not corpos-toolkit.** Its *target* is a
  toolkit-server action, but it is a build methodology (source-derived parity
  net, clean-room sanitization, coverage gate) that would read the same for any
  owned substrate. Substrate-as-target ≠ surface-required. Grouped with
  `code-migration-discipline` / `refactoring-discipline`.

If either perception (name-pairing, substrate-flavor) is later judged to
outweigh the dependency test, moving them to corpos-toolkit is a one-row edit
here plus a T3 sync — but the rule as written keeps them in corpos.

## Provenance

- Rule + rulings: chain 444 T1, sessions 13c4ecfc (2026-07-16) + e643832a (2026-07-21).
- `bug-filing` / `suggestion-filing` corrections (2026-07-16) carried into the
  canonical copies under this chain's T2.
- Underscore-skip that makes `_template` inert: corpos bug 1194, fixed
  2026-07-21 (`699cdee`).

### Deliberate divergence from the overlay (byte-identity exception)

Two canonical copies carry inline `<!-- pii-allow -->` markers the overlay
copies do not, because corpos-toolkit publishes to a public mirror and the
overlay does not:

- `deterministic-extraction-discipline/SKILL.md` — its chain-435 worked example
  names a real private host and ssh user.
- `parse-context-first-call/SKILL.md` — a memory-read path under the operator
  home directory.

Per chain 438's design, the **private repo keeps the real strings** and
`scripts/publish-public.sh` scrubs them via `.publish-scrub-map` (which already
covers the host, home-path, and user patterns) with a fail-closed verify. The markers only waive the
commit-time `pii-scan` backstop. **T3's embed-sync should strip these markers**
(or sync the scrubbed form) so corpos's embedded copy does not carry gate cruft
into session prompts.
