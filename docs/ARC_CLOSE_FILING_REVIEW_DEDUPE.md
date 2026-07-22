# Arc-Close Filing Review — Dedupe Pass Design

Chain: `arc-close-filing-review-dedupe-and-noise-reduction` (roadmap pos 14). F1 design doc.

Companion to `docs/ARC_CLOSE_FILING_REVIEW.md` (the existing pipeline overview). This doc covers the dedupe + output-validation layer that F2/F3/F4 land on top of the current Qwen review.

## 1. Problem framing

The current arc-close filing review pipeline (`mcp__toolkit-server__work.review_arc_for_filing`) emits a Qwen-reviewed array of typed filing decisions per session arc. Each decision proposes an action (`forge_bug` / `forge_vault_note` / `forge_suggestion` / `skill_update` / `memory_write` / `nothing_to_file`) plus a payload + confidence + reasoning.

Operator-side observation (2026-05-21 retrospective): **the typical session produces 3–6 decisions, of which only ~33% are signal**; the rest are duplicates, restatements of work the operator already authored, operator-error filings, or generic boilerplate. The operator triages more than acts on the output.

Quantified baseline: across 58 historical `pending_decisions` rows (the substrate's full observable window), the substrate emitted **62 actionable typed decisions** (40 `forge_vault_note`, 11 `forge_bug`, 8 `forge_suggestion`, 3 `skill_update`; 0 `memory_write`).

## 2. Failure-mode taxonomy

Categories surfaced during the F1 audit (the labelled corpus at `measure/arc-close-corpus.jsonl`):

- **real-signal** — proposed payload was filed and adopted, OR the operator would have authored equivalently in-session.
- **A — already-filed duplicate** — proposed bug/suggestion matches an existing open or recently-closed artifact in the substrate.
- **B — same-session duplicate** — same payload proposed by two arc-close fires within one `session_id`.
- **C — test-docstring restatement** — `forge_vault_note` body is mostly a quote/paraphrase of test docstrings or commit-message bodies the operator just authored. Real content; zero novel synthesis.
- **D — operator-error filing** — `forge_bug` filed for a workaround to an operator-side mistake (binary not in expected path because the operator didn't `make build` first; etc.). Not a real defect.
- **E — settled-issue refiling** — proposed payload restates something explicitly addressed and committed in the same session arc; arc-close fires AFTER the resolution but proposes the issue again.
- **F — insufficient-payload boilerplate** — generic title like "Add a regression test for X" without naming WHICH test / scenario / coverage gap. Not actionable.

## 3. Per-category historical yield

Automated heuristic first-pass (`measure/arc-close-corpus-classify.py`) on the 62-decision corpus:

| Tag | Count | % of corpus | Notes |
|---|---|---|---|
| `C-test-docstring-restatement` | 18 | 29% | Heuristic: `>60% code-block ratio` OR `≥2 test-marker hits (// @blurb, expect(, t.Errorf, t.Fatalf, TestHandle)` OR `body < 400 chars AND no paragraph breaks`. Catches **45% of all forge_vault_note proposals**. |
| `A-already-filed` | 4 | 6% | Heuristic: Jaccard ≥ 0.4 on title-tokens vs existing artifacts of the same kind (bug_list / suggestion_list / vault index). Threshold of 0.4 is conservative; spot-check showed real duplicates at 0.25–0.30 overlap (vocab variance across re-proposals). |
| `D-operator-error` | 3 | 5% | Heuristic: regex on problem_statement for markers like "binary not found", "file not found", "forgot to run", "missing in expected location". |
| `B-same-session-duplicate` | 1 | 2% | Heuristic: same-session, same-action, Jaccard ≥ 0.4 on title+problem tokens, earlier-in-time arc-close fire. |
| **Any tag (lower bound)** | **25** | **40%** | First-pass yield from conservative thresholds. |

Untagged remaining: **37 decisions (60%)**. Manual spot-check of 8 random untagged decisions: at least 2 were genuine real-signal (cross-project vault decisions that captured real lessons), 2 were already-filed duplicates the Jaccard-0.4 threshold missed (variant vocab: "Orphan Precommit-fmt Stashes" vs the existing suggestion "Automatic cleanup of orphaned precommit-fmt stashes"; Jaccard ~0.29), and the rest were borderline.

**Estimated real-signal rate post-F2/F3/F4 deployment**: 70%+ (conservative). Lower-Jaccard threshold + improved stemming would push the heuristic catch rate from 40% toward 55–60%, leaving the remaining 40–45% genuinely actionable.

## 4. Filter mechanisms

Four additive mechanisms. Each independently gated by an env-var config flag for incremental rollout + per-mechanism A/B measurement.

### F2 — Pre-filing dedupe against existing artifacts

For each proposed decision with action in {`forge_bug`, `forge_suggestion`, `forge_vault_note`}, compute Jaccard similarity on lowercased alphanumeric+hyphen token sets between the proposed title and every existing artifact title of the same kind. Threshold default: **0.30** (tuned down from 0.40 based on the spot-check finding above).

Decision downgrade matrix:
| Original action | Similarity ≥ 0.30 | < 0.30 |
|---|---|---|
| auto_execute | → surface_for_confirm | unchanged |
| surface_for_confirm | → skip | unchanged |
| skip | unchanged | unchanged |

Output adds `deduped_against: <existing-slug>` field. Fail-open: similarity-search timeout / DB error degrades to "didn't dedupe; original decision stands."

### F3 — Same-session dedupe window

Tracks proposed payloads per `(session_id, payload_signature)` in a small SQLite side-table (or TTL-bounded in-memory cache). Payload signature normalizes title + first-N-tokens of problem_statement.

Retention default: **1 hour** (configurable). Within the window, equivalent-payload re-proposals skip with `deduped_against_session_event_id: <prior-arc-event-id>` populated.

Historical yield was only 1/62 (2%), but this is bounded by the corpus's session distribution — most decisions come from sessions with only one or two arc-close fires. Live deployment will see higher rate as session arcs lengthen.

### F4 — Output-validation rules

Per-action negative-filter rules, additive (a payload passes only if NO rule rejects):

**`forge_vault_note`** rejects when:
- `body` line ratio inside fenced code blocks exceeds **0.60**, OR
- `body` contains ≥2 test-marker substrings: `// @blurb`, `expect(`, `t.Errorf`, `t.Fatalf`, `TestHandle`, `t.Run`, `func Test`, OR
- `body` length < 400 chars AND no paragraph break (`\n\n`).

**`forge_bug`** rejects when:
- `problem_statement` contains operator-error markers: "binary not found", "file not found", "forgot to run", "missing in expected location", "not found in expected", "before the command could be executed", "required a workaround", "had to be rebuilt", OR
- `problem_statement` is essentially a verbatim copy of a commit message line from the same arc summary (Levenshtein ratio > 0.85 on any single line).

**`forge_suggestion`** rejects when:
- `title` matches generic boilerplate: `^Add (a )?regression test for [A-Za-z\-_]+$` (without specifics in `problem_statement` > 200 chars), OR `^Refactor [A-Za-z\-_]+$`, OR `^Document [A-Za-z\-_]+$`, OR
- `source` field contains literal `YYYY-MM-DD` placeholder (belt-and-suspenders for pre-commit-b870665 arcs).

Output adds `validation_rejected_reason: <code>` field on any rejected decision.

### F5 — Confidence-threshold (no separate task; documented decision)

Confidence threshold stays at 0.85 for auto_execute. Today's 9 in-session filings all came in at 0.85–0.95. The threshold isn't the dial; prior-art (F2) + same-session (F3) + output-validation (F4) are. Raising the threshold would lose real signals without proportional noise reduction.

## 5. Combined yield estimate

If all three filter mechanisms (F2/F3/F4) ship with the thresholds above:

- F4 alone catches an estimated **21–25 decisions** (the 18 C-test-docstring + 3 D-operator-error + some F-boilerplate that wasn't caught by my narrow regex).
- F2 catches an estimated **6–10 decisions** (the 4 caught at 0.40 + spot-checked-as-real-duplicate count of ~4 more at 0.25–0.30).
- F3 catches an estimated **1–3 decisions** (current corpus shows 1, but live deployment will see more).

Overlap is non-trivial (a same-session duplicate is often also already-filed). Conservative non-overlapping estimate: **25–35 of 62 decisions filtered (40–55%)**.

Post-rollout target: signal-to-noise > 70%, vs the current ~33%. Acceptable threshold per chain F5's retrospective ACs: > 60% with category-level targets met.

## 6. F2 / F3 / F4 scope handoff

F2 reads this matrix + `measure/arc-close-corpus.jsonl`. Implementation:
- Add the similarity-search pass in `go/internal/arcreview/compose.go` (or a sibling file) before the typed-decisions are written to `pending_decisions`.
- Threshold value: read from `TOOLKIT_ARCCLOSE_DEDUPE_JACCARD_THRESHOLD` env var, default 0.30.
- `deduped_against` field added to the per-decision JSON shape.

F3 reads this matrix + corpus. Implementation:
- New `arc_close_session_dedupe` side-table OR in-process LRU cache keyed by `(session_id, payload_signature)`.
- Retention: `TOOLKIT_ARCCLOSE_DEDUPE_SESSION_RETENTION` env var, default `1h`.
- `deduped_against_session_event_id` field added.

F4 reads this matrix + corpus. Implementation:
- Validation rules collected in `go/internal/arcreview/validation.go` (new file).
- Each rule tagged with `// rule justification: <category from F1 corpus>` comments.
- `validation_rejected_reason` field added.

F5 measures the live arc-close fires post-rollout (N ≥ 5) and produces the closing ArchitectureAuditCompleted event.

## 7. Notes for future tuning

- **Jaccard threshold (F2)**: Start at 0.30. If false-positive rate (real signals getting downgraded) exceeds 10% in live use, raise to 0.40. If false-negative rate (duplicates passing through) stays high, lower to 0.20 with the side-effect of more `surface_for_confirm` requests (which still let real signals through after a one-click).
- **Test-marker list (F4)**: The current list (`// @blurb`, `expect(`, `t.Errorf`, `t.Fatalf`, `TestHandle`, etc.) is Go + JS centric. Add Rust markers (`assert!`, `#[test]`, `panic!`) when Rust-side test commits start producing arc-close noise.
- **The 0% memory_write count**: indicates the current Qwen prompt rarely proposes memory_write decisions. If that changes after a prompt tuning pass, consider a fifth output-validation rule (e.g., reject memory_write if the proposed memory file path already exists with similar content).
- **The 60%-untagged decisions**: spot-check sampling suggests ~half are real signal, ~half are duplicates the conservative heuristics missed. F2's threshold lowering to 0.30 should catch most of the missed duplicates. The remaining real-signal half is the floor; the filing pipeline cannot drop below that without losing useful filings.
