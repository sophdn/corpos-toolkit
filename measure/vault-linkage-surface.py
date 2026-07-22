#!/usr/bin/env python3
"""
T6 vault linkage-surface — propose cross-subdir [[wikilink]]
connections to populate the Obsidian graph.

Per 2026-05-21 user directives:
- Two-phase: this is Phase 1 (cross-subdir links between
  decisions / learnings/{general,mcp-servers,seed-packet} /
  reference). Phase 2 (intra-subdir) deferred.
- Wikilinks WITH one-line context: `- [[slug]] — <title>`
- Conservative auto-apply: Jaccard >= 0.35; surface 0.20-0.35.

Pipeline:
1. Parse all 227 in-scope entries; tokenize body.
2. For each entry, score every entry in OTHER subdirs by Jaccard
   similarity on body tokens (top 800 chars for efficiency).
3. Take top K = 6 cross-subdir neighbours per entry above
   threshold 0.20.
4. Emit corpus row per entry with proposed_links sorted by score
   descending. Each link carries tier="auto" (>=0.35) or
   tier="review" (0.20-0.35).

Output: measure/vault-linkage-corpus.jsonl

NO file mutations. The apply script reads this corpus and writes
the auto-tier links into a `## Related` section in each entry.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
OUT_PATH = Path(__file__).resolve().parent / "vault-linkage-corpus.jsonl"
PHASE = "cross"  # overridden by --phase CLI arg in main()

IN_SCOPE_SUBDIRS = [
    "decisions",
    "learnings/general",
    "learnings/mcp-servers",
    "learnings/seed-packet",
    "reference",
]

AUTO_THRESHOLD = 0.35
REVIEW_THRESHOLD = 0.20
TOP_K_PER_ENTRY = 6

# Tokenizer — reuses the triage / improvement script vocabulary.
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

FRONTMATTER_RE = re.compile(r"^---\n(.*?)\n---\n(.*)", re.DOTALL)
BODY_H1_RE = re.compile(r"^# ([^\n]+)", re.MULTILINE)


def tokens(s: str) -> set[str]:
    if not s:
        return set()
    raw = TOKEN_RE.findall(s.lower())
    return {t for t in raw if t not in STOPWORDS}


def jaccard(a: set[str], b: set[str]) -> float:
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


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
    return {"frontmatter": fm, "body": body, "raw": text}


def context_line(entry: dict) -> str:
    """Derive the one-line context for a wikilink — the
    short-form title that follows the `[[slug]] — ` prefix."""
    title = entry["frontmatter"].get("title", "").strip()
    if title:
        # Strip common prefixes for brevity
        for prefix in ("Decision: ", "Learning: ", "Reference: "):
            if title.startswith(prefix):
                title = title[len(prefix):]
        return title[:120]
    # Fallback: body H1
    m = BODY_H1_RE.search(entry["body"])
    if m:
        h1 = m.group(1).strip()
        for prefix in ("Decision: ", "Learning: ", "Reference: ", "# "):
            if h1.startswith(prefix):
                h1 = h1[len(prefix):]
        return h1[:120]
    # Fallback: filename
    return entry["filename"][:120]


def slug_from_filename(filename: str) -> str:
    """Return the Obsidian wikilink slug — filename without `.md`."""
    return filename.removesuffix(".md")


WIKILINK_RE = re.compile(r"\[\[([^\]]+)\]\]")


def existing_wikilinks(body: str) -> set[str]:
    """Slugs the entry already links to (parse-only, no mutation)."""
    return set(WIKILINK_RE.findall(body))


def main() -> int:
    global PHASE
    parser = argparse.ArgumentParser()
    parser.add_argument("--phase", choices=["cross", "intra"], default="cross",
                        help="cross: links between different subdirs (Phase 1); "
                             "intra: links within the same subdir (Phase 2)")
    args = parser.parse_args()
    PHASE = args.phase
    print(f"phase: {PHASE} ({'cross-subdir links only' if PHASE == 'cross' else 'intra-subdir links only'})", file=sys.stderr)

    # Pass 1: load every entry, tokenize TITLE + body H1 + first
    # paragraph. Title-weighted tokens give a sharper signal than
    # full-body for cross-subdir comparison — full-body dilutes
    # the load-bearing vocabulary across long prose; titles carry
    # the topic verbatim and the body's first paragraph carries
    # the load-bearing claim.
    entries = []
    for subdir in IN_SCOPE_SUBDIRS:
        full = VAULT_ROOT / subdir
        if not full.exists():
            continue
        for path in sorted(full.iterdir()):
            if not path.is_file() or path.suffix != ".md":
                continue
            try:
                parsed = parse_vault_file(path)
            except Exception as e:
                print(f"ERROR parsing {path}: {e}", file=sys.stderr)
                continue
            title = parsed["frontmatter"].get("title", "")
            body = parsed["body"]
            # Title + first 400 chars of body. Title carries the
            # topic; the first paragraph carries the load-bearing
            # claim. Going wider (full body) dilutes cross-subdir
            # comparisons; tighter (title-only) misses entries
            # whose title is a short alias.
            tok = tokens(title + " " + body[:400])
            entries.append({
                "path": str(path.relative_to(VAULT_ROOT.parent)),
                "abs_path": path,
                "subdir": subdir,
                "filename": path.name,
                "slug": slug_from_filename(path.name),
                "tokens": tok,
                "frontmatter": parsed["frontmatter"],
                "body": parsed["body"],
                "existing_links": existing_wikilinks(parsed["body"]),
            })

    print(f"loaded {len(entries)} entries", file=sys.stderr)

    # Pass 2: for each entry, score against every other entry.
    # Phase mode controls whether intra-subdir pairs are included.
    intra_subdir_enabled = (PHASE == "intra")
    rows = []
    total_auto = 0
    total_review = 0
    for i, e in enumerate(entries):
        scored = []
        for j, other in enumerate(entries):
            if i == j:
                continue
            if other["subdir"] == e["subdir"] and not intra_subdir_enabled:
                continue  # cross-subdir-only mode
            if other["subdir"] != e["subdir"] and intra_subdir_enabled:
                continue  # intra-subdir-only mode (cross-subdir was Phase 1)
            if other["slug"] in e["existing_links"]:
                continue  # already linked
            score = jaccard(e["tokens"], other["tokens"])
            if score < REVIEW_THRESHOLD:
                continue
            scored.append((score, other))
        # Top K
        scored.sort(key=lambda x: -x[0])
        top = scored[:TOP_K_PER_ENTRY]
        if not top:
            continue
        proposed_links = []
        for score, other in top:
            tier = "auto" if score >= AUTO_THRESHOLD else "review"
            proposed_links.append({
                "target_slug": other["slug"],
                "target_subdir": other["subdir"],
                "score": round(score, 3),
                "tier": tier,
                "context": context_line({
                    "frontmatter": other["frontmatter"],
                    "body": other["body"],
                    "filename": other["filename"],
                }),
            })
            if tier == "auto":
                total_auto += 1
            else:
                total_review += 1
        rows.append({
            "path": e["path"],
            "subdir": e["subdir"],
            "filename": e["filename"],
            "slug": e["slug"],
            "existing_link_count": len(e["existing_links"]),
            "proposed_links": proposed_links,
        })

    OUT_PATH.write_text("\n".join(json.dumps(r) for r in rows) + "\n")
    print(f"wrote {OUT_PATH} ({len(rows)} entries with proposed links)", file=sys.stderr)
    print(f"  auto-tier links (Jaccard >= {AUTO_THRESHOLD}): {total_auto}", file=sys.stderr)
    print(f"  review-tier links ({REVIEW_THRESHOLD} <= Jaccard < {AUTO_THRESHOLD}): {total_review}", file=sys.stderr)

    # Per-subdir summary
    print("\nentries with at least one proposed link, by subdir:", file=sys.stderr)
    from collections import Counter
    by_subdir = Counter(r["subdir"] for r in rows)
    for sd in IN_SCOPE_SUBDIRS:
        total_in_subdir = sum(1 for e in entries if e["subdir"] == sd)
        print(f"  {sd}: {by_subdir.get(sd, 0)} of {total_in_subdir}", file=sys.stderr)

    # Top 10 highest-Jaccard proposed links for spot-check
    all_links = [(r["filename"], pl) for r in rows for pl in r["proposed_links"]]
    all_links.sort(key=lambda x: -x[1]["score"])
    print("\ntop 10 highest-Jaccard proposed links (auto-tier samples):", file=sys.stderr)
    for fn, pl in all_links[:10]:
        print(f"  {pl['score']:.3f} {fn[:50]} → {pl['target_slug'][:50]}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
