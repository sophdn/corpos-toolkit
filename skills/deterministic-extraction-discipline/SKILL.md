---
name: deterministic-extraction-discipline
description: "Process for extracting crystallized, deterministic facts out of the soft RAG corpus (vault + memory) into an owned deterministic service with an exact-answer query, wiring that answer into the orient-time surface (parse_context), and retiring the soft entries to pointers so exactly one source of truth remains. Fires when a RECURRING RETRIEVAL-MISS FRICTION appears on facts that have exactly one right answer — a cold agent wrongly denies/hedges, the user corrects, the agent digs, then proceeds. Codifies the signal, the extract-vs-keep test, the determinism-vs-RAG boundary, the guardrails (ship-empty/tenant-agnostic, unknown-not-no, credential-pointers-not-secrets, owned-write-path retirement), the cold-agent verification, and a candidate-detection heuristic for sweeping the corpus. Worked example: chain 435 local-ecosystem service in corpos-toolkit. Cross-project: lives at ~/.claude/skills/."
triggers: ["deterministic extraction", "retrieval miss friction", "agent keeps missing this fact", "crystallized facts in the vault", "extract facts into a service", "correction loop on a lookup", "one source of truth for", "deterministic vs RAG", "should this be a service or stay soft", "cold agent got this wrong again"]
---

# Deterministic Extraction Discipline

The process for turning a **recurring retrieval-miss friction** into a **deterministic
lookup**. Some facts have exactly one right answer ("do I have access to example-host?", <!-- pii-allow: worked-example infra, scrubbed on publish per .publish-scrub-map -->
"what port is X on?", "which gitea org owns Y?") but live only as soft vault/memory prose
retrieved probabilistically — so a cold agent misses them, gives a wrong/hedged answer, gets
corrected, digs, and proceeds. This skill extracts those facts into an owned deterministic
service, wires the answer into orient-time, and retires the soft duplicates to pointers.

IN scope: crystallized, enumerable, exactly-one-right-answer facts across the vault + memory
RAG corpus. OUT of scope: procedures, rationale, judgment calls, and anything with more than
one defensible answer — those legitimately STAY soft in the vault (this skill draws the line
explicitly). Defers to `content-routing` for surface choice and `vault-filing-discipline` /
the global CLAUDE.md Memory section for the owned write paths.

## Core

Apply when a fact keeps being answered wrong/hedged on retrieval AND has exactly one right answer.

1. **SIGNAL (the trigger).** A recurring retrieval-miss loop: cold agent wrongly denies/hedges a
   factual query → user corrects → agent digs (vault_search/memory_read) → proceeds. The fact
   EXISTS as soft prose; retrieval just misses it. One instance is a papercut; the RECURRENCE is
   the signal to extract.
2. **EXTRACT-vs-KEEP test.** Extract a fact iff ALL hold: (a) **exactly one right answer** (not a
   judgment call); (b) **queried repeatedly** (recurring, not one-off); (c) **currently soft**
   (lives only as RAG prose); (d) **a miss causes a correction loop or a wrong action**. If it's a
   procedure, rationale, "why", or has multiple defensible answers → KEEP it as RAG.
3. **determinism-vs-RAG BOUNDARY.** The service answers **whether / where / how-to-reach** (the
   crystallized facts). The vault keeps **how-to-do / why** (procedures, gotchas, decisions). A
   service record links back to the soft note via a `soft_ref` pointer. Test: *"Is there exactly
   one correct answer an agent must never guess or hedge on?"* → deterministic. *"Is it a
   procedure, rationale, or judgment?"* → RAG.
4. **MOVE (the build).** (i) A small **typed store** (reuse existing machinery — a table/store,
   not a parallel system — if one fits). (ii) A **deterministic query** action that returns the
   exact answer + a pre-composed answer sentence. (iii) **Orient-time wiring**: register a
   detector+resolver so `parse_context` (the first-call reflex) surfaces the answer at orienting,
   NOT only via an explicit action an agent must think to call — THIS is what kills the cold-agent
   loop. (iv) **Retire** the soft entries to pointers (keep the RAG half; redirect the
   deterministic half to the service).
5. **GUARDRAILS (non-negotiable).**
   - **Ship empty / tenant-agnostic.** No tenant's data hardcoded in code/schema. Facts are LEARNED
     as data via a learn/configure action; a fresh adopter starts empty.
   - **Unknown ≠ No.** An un-learned target returns `status: unknown` ("not configured, learn it"),
     NEVER a hallucinated "no". This is the empty-tenant honesty guard.
   - **Pointers, not secrets.** Store credential/secret *pointers* (a path or env name), never the
     secret value; reject inline-secret-looking values.
   - **Owned write path for retirement.** Retire soft entries through their OWNED write surface
     (`record` for memory, `forge`/Edit for vault) — NEVER a raw write to a hook-owned dir.
   - **One source of truth.** After retirement, a search for the extracted fact must not surface a
     COMPETING soft answer — the soft entry redirects to the service (keeps only its RAG half).
6. **VERIFY with a COLD agent.** Prove the friction is gone: a fresh session, given only the query
   and no answer/correction, answers correctly on the FIRST try with no denial and no correction
   loop, by hitting the deterministic path. That is the acceptance test — not "the code compiles".

## Headline rule: orient-time wiring is what removes the loop

An explicit query action only helps an agent that thinks to call it. The friction is a
**retrieval miss at orient-time**, so the deterministic answer must reach the agent while it
ORIENTS — register the extraction as a `parse_context` reference shape (detector + resolver
sharing the same resolver function as the explicit query, so the two can never diverge). A cold
agent then gets the answer for free before it can wrongly deny. Skipping this step leaves the
loop half-open: the service exists but cold agents still miss it.

## Worked example: chain 435 local-ecosystem service (corpos-toolkit)

The signal: agents kept wrongly denying "do I have access to example-host?" (the facts — ssh <!-- pii-allow -->
user `youruser` not `sophi`, key pointer, tailnet address — lived across 8 soft vault/memory <!-- pii-allow: worked-example infra, scrubbed on publish per .publish-scrub-map -->
entries). The move:

- **Store:** reused the existing shared-infra `hosts` table + new `ecosystem_{host_addresses,
  services,access_methods}` tables (direct-write, tenant-agnostic, ships empty).
- **Query:** `ecosystem.access_check(target)` → `status: yes|no|unknown` + access methods +
  credential *pointers* + a composed answer, plus `describe`/`list`. Learn actions
  (`host_learn`/`service_learn`/`access_learn`) populate it as data.
- **Orient-time:** a `refresolve` `ShapeEcosystemToken` detector + a resolver calling the SAME
  `ResolveAccess` as the action, so `parse_context` surfaces the answer (11ms, `use_directly`).
- **Retire:** the 8 soft entries slimmed via the owned `record`/Edit paths — each now LEADS with a
  redirect banner to the service and KEEPS its how-to prose (rsync method, deploy flow, API recipe).
- **Verify:** a cold subagent answered "Yes — ssh youruser@... (key ~/.ssh/id_ed25519)" on the first <!-- pii-allow: worked-example infra, scrubbed on publish per .publish-scrub-map -->
  try, no correction, 2 tool calls. Loop eliminated.

## Candidate-detection heuristic (for sweeping the RAG corpus)

To find OTHER extraction targets across the vault + memory corpus, rank each cluster of soft
facts by:

- **Determinism** — exactly-one-right-answer? (hard gate; drop anything judgment-shaped)
- **Query frequency / recurrence** — how often is it asked, and has a miss recurred?
- **Miss cost** — does a retrieval miss cause a wrong denial / wrong action / correction loop?
- **Enumerability** — is the fact set small and closed enough for a typed store?
- **Data readiness** — do the facts already exist as prose ready to load as data?
- **Retirement cost** — how much soft prose must be slimmed, and does it have a clean RAG half to keep?

High on the first three + enumerable + ready = a strong extraction candidate. File the ranked
register to the suggestion/roadmap surface; each candidate carries its own retirement plan.
Reserve extraction for genuine recurring friction — not every soft fact wants a service; the
RAG corpus is the right home for everything that isn't a crystallized, hot, exactly-one-answer lookup.
