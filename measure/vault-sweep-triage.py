#!/usr/bin/env python3
"""
T6 vault-sweep triage — RETIRED 2026-05-21 (kept as a documented
dead-end; do NOT use the retire dispositions this script produces).

Heuristic-based triage attempted to classify vault entries into
keep / retire / relocate-to-skill based on F4-style noise patterns,
structural signals, and skill-body-token overlap. After multiple
rule-tuning passes, spot-checks against the proposed retire set
showed an 87.5% false-positive rate (14 of 16 retires were
substantive content that should be kept).

Conclusion: heuristic classification of vault value is unreliable.
The patterns that signal "valuable vault content" are diverse
enough (numbered enumerations, problem/fix patterns, when-to-apply
blocks, cross-references via prose rather than wikilinks) that no
small rule set tracks the value an operator assigns.

This script remains as documentation of the dead-end approach.
The cluster-only signals (best_skill_match, body_length, h2_count,
wikilink_count) may be useful for SURFACE work (linkage /
clarification / tagging opportunities), but should NOT drive
delete decisions.

Original behavior:
  - reads vault entries in 5 subdirs
  - emits measure/vault-sweep-corpus.jsonl with proposed
    dispositions and signals

If reusing this script for non-delete vault analysis (e.g.,
clustering or tagging), strip the disposition_for() function's
return values down to signals-only and ignore the disposition
field.
"""

from __future__ import annotations

import json
import re
import sys
import tomllib
from collections import Counter
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
VAULT_ROOT = Path.home() / ".claude" / "vault"
SKILLS_MANIFEST = REPO_ROOT / "skills" / "_manifest.toml"
OUT_PATH = REPO_ROOT / "measure" / "vault-sweep-corpus.jsonl"

IN_SCOPE_SUBDIRS = [
    "decisions",
    "learnings/general",
    "learnings/mcp-servers",
    "learnings/seed-packet",
    "reference",
]

# ---- Token / Jaccard utilities (mirrors arcreview tokenizer) -----

TOKEN_RE = re.compile(r"[a-z0-9][a-z0-9\-_]{2,}")
STOPWORDS = {
    "and", "or", "the", "for", "into", "with", "this", "that",
    "from", "session", "retro", "after", "before", "task", "bug",
    "via", "should", "may", "could", "would", "their", "are",
    "not", "but", "all", "now", "also", "have", "has", "had",
    "will", "can", "you", "your", "our", "out", "any", "some",
    "more", "most", "just", "only", "than", "then", "when", "what",
    "where", "which", "who", "how", "why", "use", "used", "using",
    "make", "made", "making", "get", "got", "getting",
}


def tokens(s: str) -> set[str]:
    if not s:
        return set()
    raw = TOKEN_RE.findall(s.lower())
    return {t for t in raw if t not in STOPWORDS}


def jaccard(a: set[str], b: set[str]) -> float:
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


# ---- F4-style noise patterns (mirrors validation.go) -------------

DIARY_STARTER_RES = [
    re.compile(r"^\s*this note captures (the|some|a)?", re.IGNORECASE),
    re.compile(r"^\s*documenting (the|a|some)?", re.IGNORECASE),
    re.compile(r"^\s*(this|the) (decision|learning|reference) documents", re.IGNORECASE),
]

OUTCOME_PARAPHRASE_RES = [
    re.compile(r"was tested.{0,40}showing \d+%", re.IGNORECASE),
    re.compile(r"improvement over .{0,40}baseline", re.IGNORECASE),
    re.compile(r"successfully (implemented|tested|committed|landed|built|ran|verified)", re.IGNORECASE),
    re.compile(r"were (successfully )?(implemented|tested|committed|landed|added)", re.IGNORECASE),
]

TEST_MARKERS = [
    "// @blurb", "expect(", "t.Errorf", "t.Fatalf", "t.Run",
    "TestHandle", "func Test", "assert!(", "#[test]",
]


def code_block_ratio(body: str) -> float:
    if not body:
        return 0.0
    lines = body.split("\n")
    in_block = False
    inside = 0
    for line in lines:
        if line.strip().startswith("```"):
            in_block = not in_block
            continue
        if in_block:
            inside += 1
    return inside / max(1, len(lines))


# ---- Skill manifest loading --------------------------------------

def load_skills() -> list[dict]:
    """Return list of {name, body_path, bucket, trigger_keywords,
    description, signature (token set)}. Signature combines:
    manifest fields (name, triggers, description) + first ~3KB of
    the skill's body (SKILL.md). The body provides the prose
    vocabulary that overlaps better with vault entries; manifest
    fields alone produce Jaccard < 0.10 for almost all entries."""
    with SKILLS_MANIFEST.open("rb") as f:
        manifest = tomllib.load(f)
    out = []
    for s in manifest.get("skill", []):
        triggers = s.get("trigger_keywords") or []
        body_path = s.get("body_path", "")
        body_text = ""
        if body_path:
            body_md_paths = [
                REPO_ROOT / body_path / "SKILL.md",
                REPO_ROOT / body_path,
            ]
            for p in body_md_paths:
                if p.exists() and p.is_file():
                    body_text = p.read_text(encoding="utf-8", errors="replace")[:3000]
                    break
        sig = tokens(
            s.get("name", "") + " " +
            " ".join(triggers) + " " +
            s.get("description", "") + " " +
            body_text
        )
        out.append({
            "name": s.get("name", ""),
            "body_path": body_path,
            "bucket": s.get("bucket", ""),
            "trigger_keywords": triggers,
            "description": s.get("description", ""),
            "signature": sig,
        })
    return out


# ---- Vault entry parsing -----------------------------------------

FRONTMATTER_RE = re.compile(r"^---\n(.*?)\n---\n(.*)", re.DOTALL)


def parse_vault_file(path: Path) -> dict:
    text = path.read_text(encoding="utf-8", errors="replace")
    m = FRONTMATTER_RE.match(text)
    if m:
        fm_raw, body = m.group(1), m.group(2)
        fm = {}
        for line in fm_raw.splitlines():
            if ":" in line and not line.startswith(" "):
                k, _, v = line.partition(":")
                fm[k.strip()] = v.strip().strip('"')
    else:
        fm, body = {}, text
    return {
        "frontmatter": fm,
        "body": body,
        "raw": text,
    }


WIKILINK_RE = re.compile(r"\[\[([^\]]+)\]\]")
H2_HEADER_RE = re.compile(r"^## [^\n]+", re.MULTILINE)

# Substantive section names that signal curated decision/learning
# content rather than template scaffolding (## Title / ## Body alone
# is the empty-template shape). T6 spot-check 2026-05-21 found that
# the three false-positive retires all carried multiple of these.
SUBSTANTIVE_SECTION_NAMES = {
    "decision", "context", "background", "rationale", "trade-off",
    "trade-offs", "tradeoffs", "tradeoff", "when to use",
    "when NOT to use", "when not to use", "counter-signal",
    "counter signal", "counter-signals", "how to apply",
    "encountered", "frictions", "frictions to expect",
    "related", "see also", "cross-references", "cross references",
    "references", "open questions", "supersession",
    "supersedes", "superseded by", "lessons", "lesson",
    "pattern", "failure modes", "failure mode", "signals",
    "signal", "guard", "guards", "why", "what", "how",
    "next", "next steps", "follow-up", "follow up", "follow-ups",
    "scope", "out of scope", "in scope", "implementation",
    "verification", "validation",
}
SUBSTANTIVE_SECTION_RE = re.compile(
    r"^## (" + "|".join(re.escape(n) for n in sorted(SUBSTANTIVE_SECTION_NAMES, key=len, reverse=True)) + r")\b",
    re.IGNORECASE | re.MULTILINE,
)


def wikilink_count(body: str) -> int:
    return len(WIKILINK_RE.findall(body))


def h2_header_count(body: str) -> int:
    return len(H2_HEADER_RE.findall(body))


def substantive_section_count(body: str) -> int:
    return len(SUBSTANTIVE_SECTION_RE.findall(body))


def has_related_section(body: str) -> bool:
    return bool(re.search(
        r"^## (related|see also|cross[- ]?references|references)\b",
        body, re.IGNORECASE | re.MULTILINE,
    ))


# ---- Disposition logic -------------------------------------------

# Thresholds (aggressive-retire per user directive 2026-05-21).
SHORT_BODY_THRESHOLD = 1500   # bodies < this need extra signal to keep
SUBSTANTIVE_BODY_THRESHOLD = 3500
PARAGRAPH_BREAK = "\n\n"
HIGH_WIKILINK_THRESHOLD = 3
SKILL_OVERLAP_THRESHOLD = 0.12  # tuned against the body+manifest skill
                                # signature; vault-to-skill prose overlap
                                # peaks ~0.20-0.30 with the richer
                                # signature.


def disposition_for(
    path: Path,
    parsed: dict,
    skills: list[dict],
) -> dict:
    fm = parsed["frontmatter"]
    body = parsed["body"]
    title = fm.get("title", path.stem)
    body_opener = body[:120].strip()
    signals = []

    # --- F4 noise patterns (instant retire) ---
    for re_ in DIARY_STARTER_RES:
        if re_.search(title) or re_.search(body_opener):
            return {
                "disposition": "retire",
                "rationale": "diary-style framing: 'This note captures...' / 'Documenting...' opener — content IS the just-committed work, not a cross-project pattern",
                "signals": ["diary-opener"],
                "primary_signal": "diary_opener",
            }
    for re_ in OUTCOME_PARAPHRASE_RES:
        if re_.search(body):
            return {
                "disposition": "retire",
                "rationale": "outcome paraphrase: body contains 'was tested showing X%', 'successfully implemented', 'improvement over baseline' shape — narrating commit not synthesising",
                "signals": ["outcome-paraphrase"],
                "primary_signal": "outcome_paraphrase",
            }
    marker_hits = sum(1 for m in TEST_MARKERS if m in body)
    if marker_hits >= 2:
        return {
            "disposition": "retire",
            "rationale": f"test-restatement: body contains {marker_hits} test-marker substrings (t.Errorf / TestHandle / etc.) — paraphrases test code, not cross-project lesson",
            "signals": [f"test-markers={marker_hits}"],
            "primary_signal": "test_restatement",
        }
    code_ratio = code_block_ratio(body)
    if code_ratio > 0.60:
        return {
            "disposition": "retire",
            "rationale": f"code-heavy: {code_ratio:.0%} of body inside fenced code blocks; the code itself is the durable artifact, no synthesis to retain",
            "signals": [f"code-ratio={code_ratio:.2f}"],
            "primary_signal": "high_code_ratio",
        }

    # --- Structured-content early-keep (added 2026-05-21 after T6
    # spot-check revealed false-positive retires on substantive
    # multi-section content lacking explicit cross-project markers).
    # Discriminator: count H2 sections with NAMED-SUBSTANTIVE titles
    # (## Decision, ## Context, ## Related, ## When to use, etc.).
    # Raw H2 count is too lenient because vault template scaffolding
    # contributes ## Title / ## Body. Threshold: 2+ substantive
    # section names OR explicit Related/See-also section. ---
    substantive_h2 = substantive_section_count(body)
    has_related = has_related_section(body)
    h2_total = h2_header_count(body)
    # Three independent paths to structured-keep:
    #   (a) 2+ recognized substantive section names (## Decision, ## Context, etc).
    #   (b) Explicit Related / See also / Cross-references section.
    #   (c) 5+ raw H2 headers + body >= 2000 chars — catches numbered-list
    #       enumerations ("## 1. Foo / ## 2. Bar / ...") that don't match the
    #       named-section regex but ARE structured. Length floor excludes
    #       template-only entries that hit 5+ H2s via scaffolding alone.
    if substantive_h2 >= 2 or has_related or (h2_total >= 5 and len(body) >= 2000):
        return {
            "disposition": "keep",
            "rationale": f"curated multi-section content: {substantive_h2} substantive section names, related-section={has_related}, h2-total={h2_total}, len={len(body)}",
            "signals": [f"substantive-h2={substantive_h2}", f"related={has_related}", f"h2={h2_total}"],
            "primary_signal": "keep_structured",
        }

    # --- Length-based retire (aggressive) ---
    if len(body) < 800 and PARAGRAPH_BREAK not in body:
        return {
            "disposition": "retire",
            "rationale": f"single-thought entry: body {len(body)} chars with no paragraph break — too narrow for cross-project value",
            "signals": [f"len={len(body)}"],
            "primary_signal": "single_thought",
        }
    if len(body) < SHORT_BODY_THRESHOLD and wikilink_count(body) == 0:
        # Short AND no cross-references → probably context-bound to a single
        # incident; aggressive-retire unless skill-relocate target found.
        skill_match = best_skill_match(title, body, skills)
        if skill_match and skill_match["score"] >= SKILL_OVERLAP_THRESHOLD:
            return {
                "disposition": f"relocate-to-skill",
                "target_skill": skill_match["name"],
                "rationale": f"short ({len(body)} chars) and uncross-referenced; high overlap with skill '{skill_match['name']}' (Jaccard {skill_match['score']:.2f}) — fits the skill body better than the vault",
                "signals": [f"len={len(body)}", "no-wikilinks", f"skill-match={skill_match['name']}"],
                "primary_signal": "relocate_to_skill_short",
            }
        return {
            "disposition": "retire",
            "rationale": f"short ({len(body)} chars), no cross-references, no skill-relocate target — context-bound to a single past incident; unlikely cross-project value going forward",
            "signals": [f"len={len(body)}", "no-wikilinks"],
            "primary_signal": "short_uncross_referenced",
        }

    # --- Skill relocate (substantive but skill-shaped) ---
    skill_match = best_skill_match(title, body, skills)
    if skill_match and skill_match["score"] >= SKILL_OVERLAP_THRESHOLD:
        # Even substantive entries can be skill-shaped (procedural how-to,
        # convention guide). If the skill-fit is strong, prefer relocate.
        # But check for cross-project synthesis first — if the body
        # explicitly names cross-project patterns, keep instead.
        cross_project_signals = sum(
            1 for marker in [
                "cross-project", "every project", "across projects",
                "any project", "all projects", "generalises across",
                "pattern across", "future agents", "future sessions",
            ]
            if marker.lower() in body.lower()
        )
        if cross_project_signals >= 2:
            signals.append(f"cross-project-signals={cross_project_signals}")
            return {
                "disposition": "keep",
                "rationale": f"substantive cross-project synthesis: {cross_project_signals} cross-project markers in body; skill-overlap exists ({skill_match['name']} {skill_match['score']:.2f}) but synthesis-shape dominates",
                "signals": signals + [f"skill-overlap={skill_match['name']}@{skill_match['score']:.2f}"],
                "primary_signal": "keep_substantive_cross_project",
            }
        return {
            "disposition": "relocate-to-skill",
            "target_skill": skill_match["name"],
            "rationale": f"skill-shaped procedural content: high overlap ({skill_match['score']:.2f}) with skill '{skill_match['name']}'; belongs in skill body, not vault",
            "signals": [f"skill-match={skill_match['name']}@{skill_match['score']:.2f}"],
            "primary_signal": "relocate_to_skill_substantive",
        }

    # --- Keep cases ---
    cross_project_signals = sum(
        1 for marker in [
            "cross-project", "every project", "across projects",
            "any project", "all projects", "generalises across",
            "pattern across", "future agents", "future sessions",
        ]
        if marker.lower() in body.lower()
    )
    wikilinks = wikilink_count(body)
    if cross_project_signals >= 1 or wikilinks >= HIGH_WIKILINK_THRESHOLD or len(body) >= SUBSTANTIVE_BODY_THRESHOLD:
        return {
            "disposition": "keep",
            "rationale": f"substantive: {len(body)} chars, {wikilinks} wikilinks, {cross_project_signals} cross-project markers",
            "signals": [f"len={len(body)}", f"wikilinks={wikilinks}", f"cross-project={cross_project_signals}"],
            "primary_signal": "keep_substantive",
        }

    # --- Ambiguous fallback ---
    return {
        "disposition": "retire",
        "rationale": f"borderline: {len(body)} chars, {wikilinks} wikilinks, no skill-relocate target, no strong cross-project markers — aggressive-retire per 2026-05-21 directive",
        "signals": [f"len={len(body)}", f"wikilinks={wikilinks}"],
        "primary_signal": "borderline_no_signal",
    }


def best_skill_match(title: str, body: str, skills: list[dict]) -> dict | None:
    # Weight title heavily (skill targets are short labels); first 800 chars
    # of body capture the load-bearing context.
    entry_sig = tokens(title) | tokens(body[:800])
    if not entry_sig:
        return None
    best = None
    for s in skills:
        sim = jaccard(entry_sig, s["signature"])
        if best is None or sim > best["score"]:
            best = {"name": s["name"], "score": round(sim, 3)}
    return best


# ---- Main ---------------------------------------------------------

def main() -> int:
    skills = load_skills()
    print(f"loaded {len(skills)} skills from manifest", file=sys.stderr)

    out_rows = []
    for subdir in IN_SCOPE_SUBDIRS:
        full = VAULT_ROOT / subdir
        if not full.exists():
            print(f"WARN: subdir missing: {subdir}", file=sys.stderr)
            continue
        for path in sorted(full.iterdir()):
            if not path.is_file() or path.suffix != ".md":
                continue
            try:
                parsed = parse_vault_file(path)
            except Exception as e:
                print(f"ERROR parsing {path}: {e}", file=sys.stderr)
                continue
            disp = disposition_for(path, parsed, skills)
            # Always record best-skill-match — even on entries the
            # disposition logic decided to keep, the user may
            # spot-check "is there a near-skill-target I should
            # override to relocate?" using this column.
            best = best_skill_match(
                parsed["frontmatter"].get("title", ""),
                parsed["body"],
                skills,
            )
            row = {
                "path": str(path.relative_to(VAULT_ROOT.parent)),
                "subdir": subdir,
                "filename": path.name,
                "title": parsed["frontmatter"].get("title", ""),
                "date": parsed["frontmatter"].get("date", ""),
                "note_kind": parsed["frontmatter"].get("note_kind", ""),
                "topic": parsed["frontmatter"].get("topic", ""),
                "tags": parsed["frontmatter"].get("tags", ""),
                "body_length": len(parsed["body"]),
                "wikilink_count": wikilink_count(parsed["body"]),
                "h2_count": h2_header_count(parsed["body"]),
                "substantive_h2_count": substantive_section_count(parsed["body"]),
                "has_related_section": has_related_section(parsed["body"]),
                "best_skill_match": best["name"] if best else "",
                "best_skill_score": best["score"] if best else 0.0,
                **disp,
            }
            out_rows.append(row)

    OUT_PATH.parent.mkdir(parents=True, exist_ok=True)
    with OUT_PATH.open("w") as f:
        for row in out_rows:
            f.write(json.dumps(row) + "\n")
    print(f"wrote {OUT_PATH} ({len(out_rows)} rows)", file=sys.stderr)

    # Summary
    by_disp = Counter(r["disposition"] for r in out_rows)
    by_primary = Counter(r["primary_signal"] for r in out_rows)
    by_subdir = Counter(r["subdir"] for r in out_rows)

    print("\nby disposition:", file=sys.stderr)
    for d, n in by_disp.most_common():
        print(f"  {d}: {n} ({n*100//len(out_rows)}%)", file=sys.stderr)
    print("\nby primary signal:", file=sys.stderr)
    for s, n in by_primary.most_common():
        print(f"  {s}: {n}", file=sys.stderr)
    print("\nby subdir (total):", file=sys.stderr)
    for s, n in by_subdir.most_common():
        print(f"  {s}: {n}", file=sys.stderr)

    # Per-subdir × disposition breakdown
    print("\nper-subdir × disposition:", file=sys.stderr)
    for s in IN_SCOPE_SUBDIRS:
        s_rows = [r for r in out_rows if r["subdir"] == s]
        if not s_rows:
            continue
        ds = Counter(r["disposition"] for r in s_rows)
        ds_str = ", ".join(f"{d}={n}" for d, n in ds.most_common())
        print(f"  {s} ({len(s_rows)}): {ds_str}", file=sys.stderr)

    # Top 10 relocate-to-skill targets
    relocate_targets = Counter(
        r.get("target_skill", "") for r in out_rows
        if r["disposition"].startswith("relocate-to-skill") and r.get("target_skill")
    )
    if relocate_targets:
        print("\ntop relocate-to-skill targets:", file=sys.stderr)
        for tgt, n in relocate_targets.most_common(10):
            print(f"  {tgt}: {n}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
