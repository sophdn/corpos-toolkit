# Content Routing Taxonomy

Where vault notes / skills / code-comments / commit-messages / inline-docs each fit. Built from the T6 vault-sweep audit of `~/.claude/vault/` (227 in-scope entries across `decisions/`, `learnings/{general,mcp-servers,seed-packet}/`, `reference/`).

Chain: `arc-close-filing-review-dedupe-and-noise-reduction` T6. Companion to `docs/ARC_CLOSE_FILING_REVIEW_DEDUPE.md` (the dedupe-pipeline design).

> **T6 audit closeout, 2026-05-21**: heuristic triage attempted to classify vault entries for retire/keep/relocate. Spot-checks revealed an 87.5% false-positive rate on proposed retires — the vault's content is far more substantive than the heuristics could detect. Walk-back: triage script archived as a documented dead-end; no Phase 2 deletes ran. The chain's value lands in the F2/F3/F4 noise-filter pipeline (already live), this taxonomy doc, and F7's prompt update — not in vault cleanup. T6 now shapes around clarification / linkage / tagging passes against the existing substantive content rather than deletion.

## 1. The four canonical surfaces

| Surface | Owns | Reader / consumer | Decay model |
|---|---|---|---|
| **Vault** (`~/.claude/vault/decisions/`, `learnings/`, `reference/`) | Cross-project synthesis, durable lessons, supersession history | Future-me; agents on session start (via vault_pull-discipline) | Slow (years); audit pass every ~6 months |
| **Skills** (`~/.claude/skills/<name>/SKILL.md`) | Procedural how-to, convention, in-session reflex | Agent-on-trigger (parse_context emits skill_trigger → inline body) | Medium (per-quarter review); skills evolve with the work |
| **Code** (inline comments, struct doc, function-level rationale) | Why this specific code does what it does | Anyone reading the code | Lives with the code; rots on rename, refactor, removal |
| **Commit messages** | What changed in THIS diff, why now, what trade-offs | Anyone running `git log` / reading PR history | Permanent but contextual; loses signal as repo evolves |

A note proposed for the vault should fail a routing pass if any other surface fits better. The taxonomy below names the patterns and their canonical homes.

## 2. Patterns the triage tried to detect (and the spot-check failures)

The vault-sweep-triage script attempted to classify entries by structural / token / length signals. Spot-checks against the proposed retire bucket consistently surfaced false-positives. Documented here so future audit passes know the dead-ends:

| Heuristic | Why it failed |
|---|---|
| `borderline_no_signal`: substantive length, no cross-project markers, low wikilinks, body < 3500 chars | Vault content uses diverse synthesis markers — "Why this is a vault note", problem/fix structures, numbered enumerations — none captured by the marker-phrase list. |
| `substantive_section_names`: ≥ 2 H2 headers from a recognised name set (Decision/Context/Related/etc) | The recognised set was too narrow; numbered enumerations ("## 1. Recap masquerading...") and varied section names ("## The bug I introduced") all signal curated content but match no canonical name. |
| `short_uncross_referenced`: < 1500 chars + zero wikilinks + no skill-relocate target | Short entries with concrete operational value (Rust build cache sizes, kiwix-serve API shape, in-memory SQLite URL format) are still cross-applicable. Length doesn't track value. |
| `outcome_paraphrase`: past-tense outcome language ("was tested showing X%") | The only heuristic that consistently identified noise — caught a single corpus entry that paraphrased a just-committed change. F4's live filter uses this same rule. |

**Net signal**: heuristic value-classification of vault content is unreliable. The only validated-as-noise pattern is the F4 noise filters (already live in the dispatch pipeline). Vault cleanup as an audit-and-delete pass yields ~0-1% confident retires per 200 entries; not worth a sweep.

## 3. The routing decision table

For a proposed note, apply these tests in order. First-match wins.

| If the proposed note... | Route to | Rationale |
|---|---|---|
| ...is a substantive named-cross-project pattern, multi-section, references prior decisions via [[wikilinks]] | **vault decision** | The vault's purpose. |
| ...is a durable how-to or "always do X" procedural guide that fires on a trigger keyword | **skill body** (existing skill or new skill) | Agents act on triggers; vault notes don't fire reflexively. |
| ...describes the behavior of a specific function / file / module | **inline comment** in that file | Lives with the code; survives refactor only when colocated. |
| ...narrates what changed in a specific commit (X was added, Y was fixed) | **commit message body** | The commit message IS the record of "what just happened." Vault notes that restate this are pure duplication. |
| ...captures a one-off operator-workaround (binary not found; forgot to build) | **discard** | Not a pattern; just an incident. |
| ...is shorter than ~1500 chars AND has no wikilinks AND has no skill-relocate target | **discard** (or revisit the original framing) | Context-bound to a single past observation; unlikely to surface again. |
| ...uses past-tense outcome language ("was tested showing N%", "successfully implemented", "improvement over baseline") | **discard** | This shape narrates a commit; not cross-project synthesis. |
| ...starts with "This note captures..." or "Documenting..." | **discard** | Diary framing — the note IS the work, not the lesson. |

## 4. Skill-relocate target catalog

T6 surfaced 2 strong skill-relocate candidates (Jaccard > 0.12 against the body+manifest signature). The skills with the strongest gravitational pull on vault content:

| Skill | Best-matching vault entries (top examples by score) |
|---|---|
| `coding-philosophy` | `2026-05-10_extract-coding-philosophy-base-skill.md` (0.13) |
| `session-routing` | `2026-05-10_port-session-routing-rubric.md` (0.14) |
| `vault-pull-discipline` | `2026-05-08_local-llm-routing-needs-grounding.md` (0.11), `2026-05-04_mcp-architecture-design.md` (0.08) |
| `external-knowledge-retrieval` | `2026-05-20_kiwix.md` (0.10) |
| `agentic-architecture-audit` | `2026-05-04_agentic-conventions.md` (0.10) |
| `vault-filing-discipline` | `2026-05-20_vault-note-scope-vs-project-split.md` (0.08) |
| `parse-context-first-call` | several substrate-design entries at 0.06-0.09 |

The vault-to-skill token overlap is inherently lower than vault-to-vault (skill manifests use compact technical vocabulary; vault entries use prose). The 0.12 threshold caught 2 strong matches; ~10-15 additional candidates sit at 0.08-0.10 and warrant operator-review (sort `measure/vault-sweep-corpus.jsonl` by `best_skill_score` descending; spot-check the keep dispositions for "should this actually relocate?").

## 5. Lessons from the T6 audit

1. **The vault accumulated context-bound short notes more than diary paraphrases.** F4's filter set (designed against the arc-close noise patterns) catches different shapes than the vault's historical noise. A vault-specific filter set focused on "context-bound to a single past incident" would catch more.
2. **66% of historical vault content passes the substantive-keep bar.** The remaining 34% breakdown — 24% borderline-without-signal, 8% short-and-uncross-referenced, 1% relocate, 1% outcome-paraphrase — quantifies how much "soft drift" the vault has accumulated despite the existing filing discipline.
3. **Cross-project markers are a strong-but-noisy signal.** Entries with phrases like "across projects" / "every project" / "future agents" almost always pass the keep bar. The presence of even ONE such marker boosts an entry from borderline-retire to substantive-keep.
4. **Wikilinks are the highest-fidelity "this note still matters" signal.** Entries with ≥ 3 inbound wikilinks are almost certainly canonical. Entries with 0 wikilinks are almost certainly orphan or context-bound — even when their body is long. (Current vault: most entries have 0-1 wikilinks; high-wikilink entries are rare.)

## 6. F7 prompt update inputs

For F7's Qwen prompt update, this taxonomy translates into the following negative-routing rules ("if X, propose Y instead of forge_vault_note"):

- If the proposed content describes a specific commit's outcome → propose `nothing_to_file` (the commit message owns it).
- If the proposed content is a procedural "always do X" → propose `skill_update` against the matching skill from §4.
- If the proposed content is a one-off workaround for operator error → propose `nothing_to_file`.
- If the proposed content paraphrases test code or test outcomes → propose `nothing_to_file`.
- If the proposed content is < 400 words with no named cross-project pattern → propose `nothing_to_file`.

F7's implementation adds these rules between the existing PREFERENCE ORDER and SIGNAL TAXONOMY sections of `reviewSystemPrompt()`.
