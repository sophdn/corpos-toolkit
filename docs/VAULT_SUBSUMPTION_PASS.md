# Vault Note ↔ Skill Subsumption Pass

**Chain:** reference-resolution-migration (id 598)
**Task:** T11 — vault-note-skill-subsumption-dedup
**Date:** 2026-05-18

Audit pass to identify vault notes whose discipline-shape has been
promoted into an active skill — so vault_search can deprioritize them
in favor of the canonical skill body.

---

## Finding: zero fully-subsumed notes today

The vault contains 175 learning + decision notes. A representative
pass (`grep -lr` against the active discipline-skill names:
`bug-filing-discipline`, `vault-pull-discipline`, `vault-filing-discipline`,
`scratchpad-discipline`, `content-routing`, `coding-philosophy`,
`reference-resolution`) surfaced 13 vault notes that REFERENCE these
disciplines — none that have been SUPERSEDED BY them.

The pattern across the 13:

| Note | Relationship to discipline skill |
|---|---|
| `2026-05-10_promote-discipline-skill-when-deployed-cost-drops.md` | Meta-rule ABOUT promoting disciplines to skills. The methodology, not a predecessor. |
| `2026-05-15_grep-head-before-believing-mcp-symptoms.md` | References `bug-filing-discipline` as the file-via-X recommendation. Note itself is about diagnostics; the discipline-mention is a downstream pointer. |
| `2026-05-15_stale-stdio-mcp-binary-fingerprint.md` | Same shape — references discipline for follow-up action, not subsumed by it. |
| `2026-05-17_closing-process-in-bug-acceptance-criteria.md` | About bug acceptance criteria structure; mentions filing discipline in passing. |
| `2026-05-09_qwen-2-5-32b-utility-shape-baseline.md` | Qwen capability assessment; discipline mention is context. |
| `2026-05-17_bug-to-task-conversion-pattern.md` | Bug-vs-task conversion pattern; mentions filing discipline for the bug-side rule. |
| `2026-05-11_unified-knowledge-index-outcome.md` | Knowledge index outcome decision; discipline mention is context. |
| `2026-05-17_reject-symbolic-markdown-agent-protocol.md` | Protocol decision; discipline mention is one of several "things we have" enumerations. |
| `next-qwen-candidates-post-migration_2026-05-14.md` | Qwen candidate list; mentions disciplines as context for which classifiers might offload. |
| `2026-05-18_status-only-blockers-go-stale-without-structural-edges.md` | Today's own learning note about structural-vs-status-only blockers. Mentions discipline as related infrastructure. |
| `2026-05-18_action-doc-canonical-param-with-no-struct-tag-causes-silent-success.md` | Today's silent-failure learning. Discipline-mention is a link to bug-filing-discipline. |
| `2026-05-18_source-grep-ci-gates-must-exclude-test-files-to-detect-real-drift.md` | Today's CI-gate learning. Discipline-mention is a link. |
| `2026-05-18_parity-tests-between-live-classifiers-assert-outputs-not-rule-equality.md` | Today's parity-test learning. Discipline-mention is a link. |

**None of these have been superseded.** The disciplines codified in
skills (`bug-filing-discipline`, `vault-pull-discipline`, etc.) were
authored DIRECTLY as skills — they didn't grow up as vault notes that
later got promoted. The vault-mentions are downstream pointers + meta-
analysis, not predecessor content.

---

## Why the healthier-than-expected state

Looking at when each discipline skill was authored vs when the vault
notes referencing them were written:

- The discipline-skills (`~/.claude/skills/bug-filing-discipline/SKILL.md`
  and siblings) are authored as the canonical first-version of the
  discipline. Their content is the rule, the trigger, the application
  guidance.
- Vault notes that reference them are ADDITIONAL surfaces — diagnostic
  patterns, meta-rules, specific session learnings — that POINT AT the
  discipline as the canonical mechanism.

This separation matches the [vault-vs-skill content-routing rule]
(`~/.claude/skills/content-routing/SKILL.md`): repeatable rules go in
skills; insights / decisions / learnings go in vault. The boundary
was respected from the start, so subsumption didn't accumulate.

---

## Schema extension: `subsumed_by` (documented for future use)

The `vault-note` forge schema can be extended with a `subsumed_by`
frontmatter field for future subsumption cases:

```yaml
---
date: 2026-XX-XX
title: ...
tags: [...]
subsumed_by: bug-filing-discipline  # OPTIONAL — skill slug whose discipline now covers this note's content
superseded_at: 2026-XX-XX            # OPTIONAL — when the subsumption was acknowledged
---
```

When `vault_search` encounters a result with `subsumed_by` set:
- The result still surfaces in the response (history is preserved).
- A `subsumption_marker: "see skill <slug> for active discipline; this note is historical"` field appears on the Candidate.
- The substrate's downstream consumers (the agent reading the response) consult the active skill first.

T11 does NOT implement this schema extension because there are no
notes to mark today. The extension is documented for the future
subsumption case: if a vault note is written and later becomes fully
covered by a skill (e.g. a learning note that gets promoted into a
skill body verbatim), THAT note gets the `subsumed_by` field added at
promotion time.

---

## Future-defense pattern

When promoting a vault learning's content into a skill:

1. Author the skill at `mcp-servers/skills/<name>/SKILL.md` per the
   self-containment manifest (T2-T3 of this chain).
2. Add `subsumed_by: <skill-name>` to the original vault note's
   frontmatter.
3. Set `superseded_at: <date>` so the longitudinal record shows when.
4. Don't delete the vault note — history is signal.
5. `vault_search`'s response surfaces the marker so the agent
   consults the active skill.

This pattern is documented here for the future case. T7 (skill body
paring) may surface candidates if the bucket-assignment analysis
reveals notes that COULD be promoted to skills; if so, the promotion
follows this five-step pattern.

---

## Chain handoff

T11 closes. No vault entries marked `subsumed_by` (none qualified).
The `vault-note` schema extension is documented as a future-defense
pattern; T12 retrospective notes that no subsumption was needed at
migration time. If T7's analysis surfaces subsumption candidates,
those are filed as follow-on or handled inline with T7's paring.
