#!/usr/bin/env python3
"""
T6 vault improvement-surface — surface mechanical-win opportunities
across all 227 in-scope vault entries.

Focus per 2026-05-21 user directive: empty title sections +
tag normalisation. The retire/relocate path was abandoned after
spot-checks revealed 87.5% false-positive rate; the value of T6
shifts to making the existing substantive content easier to find.

Two opportunity types surfaced:

1. EMPTY-TITLE entries — `## Title` section has no text between
   the header and the next section. The body usually carries an
   H1 (`# ...`) or the frontmatter has a `title:` field that
   should populate the Title section.

2. SPARSE-TAGS entries — frontmatter `tags:` count below the
   subdir's median. Proposes additional tags drawn from peer
   entries (token-similarity neighbours in the same subdir).

Output: measure/vault-improvement-corpus.jsonl — one row per
opportunity. NO file mutations; apply phase reads the file after
operator review.

Outputs counts per opportunity-type for visibility.
"""

from __future__ import annotations

import json
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
OUT_PATH = Path(__file__).resolve().parent / "vault-improvement-corpus.jsonl"

IN_SCOPE_SUBDIRS = [
    "decisions",
    "learnings/general",
    "learnings/mcp-servers",
    "learnings/seed-packet",
    "reference",
]

# Match the canonical `## Title` ... `## Body` template. The Title
# section is empty when there's no non-whitespace between the two
# headers. Use a permissive content matcher so single-line titles
# and multi-line titles both register.
TITLE_BODY_RE = re.compile(
    r"^## Title\s*\n(.*?)\n## Body\b",
    re.DOTALL | re.MULTILINE,
)

# Find the first H1 in the body section (the `# Some heading` line
# that's the load-bearing claim). Anchored to start of line.
BODY_H1_RE = re.compile(r"^# ([^\n]+)", re.MULTILINE)

# Frontmatter parser — same as triage script.
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
    return {"frontmatter": fm, "body": body}


# ---- Opportunity 1: empty title sections -------------------------

def extract_title_section_content(body: str) -> str:
    """Return the text between `## Title` and `## Body`, stripped.
    Returns empty string when the template isn't present OR the
    section is empty."""
    m = TITLE_BODY_RE.search(body)
    if not m:
        return ""
    return m.group(1).strip()


def first_body_h1(body: str) -> str:
    """Return the first H1 line after the `## Body` marker, or
    empty string when no H1 exists."""
    # Find content after `## Body`; restrict H1 search to that span
    body_match = re.search(r"## Body\s*\n(.*)", body, re.DOTALL)
    if not body_match:
        # No ## Body section — search the whole body
        body_after = body
    else:
        body_after = body_match.group(1)
    m = BODY_H1_RE.search(body_after)
    if not m:
        return ""
    return m.group(1).strip()


def proposed_title_for_empty_section(parsed: dict, path: Path) -> tuple[str, str]:
    """Return (proposed_title, source_name) for an entry whose
    `## Title` section is empty. Source is one of:
    'frontmatter', 'body_h1', 'slug'. Empty proposed_title means no
    candidate found (caller can skip surfacing)."""
    fm_title = parsed["frontmatter"].get("title", "").strip()
    if fm_title:
        return (fm_title, "frontmatter")
    h1 = first_body_h1(parsed["body"])
    if h1:
        return (h1, "body_h1")
    # Slug fallback — humanise the filename
    slug = path.stem
    if slug.startswith("20") and "_" in slug:
        # Drop YYYY-MM-DD_ prefix
        slug = slug.split("_", 1)[1]
    humanised = slug.replace("-", " ").replace("_", " ").capitalize()
    return (humanised, "slug")


# ---- Opportunity 2: sparse tags ----------------------------------

def parse_tags(raw: str) -> list[str]:
    """Parse the tags frontmatter field. Accepts bracketed JSON-ish
    arrays ("[a, b, c]") OR space/comma-separated bare lists."""
    if not raw:
        return []
    raw = raw.strip()
    if raw.startswith("[") and raw.endswith("]"):
        raw = raw[1:-1]
    items = [t.strip().strip('"').strip("'") for t in re.split(r"[,\s]+", raw)]
    return [t for t in items if t]


# Tokenisation for tag-neighbour suggestion. Reuses the triage
# script's stopword set so neighbours computed by the two scripts
# are consistent.
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


def suggest_tags_from_neighbours(
    entry_tokens: set[str],
    current_tags: list[str],
    candidates: list[dict],
    top_k: int = 5,
    min_neighbour_score: float = 0.10,
) -> list[str]:
    """Return tag suggestions: the top tags appearing in the
    similarity-neighbours that the current entry doesn't have.
    Candidates is the full corpus minus self. Returns up to top_k
    tag suggestions."""
    neighbours = []
    for c in candidates:
        score = jaccard(entry_tokens, c["tokens"])
        if score >= min_neighbour_score:
            neighbours.append((score, c["tags"]))
    if not neighbours:
        return []
    # Tally tag occurrences in neighbours, weighted by similarity
    tag_score: dict[str, float] = defaultdict(float)
    for score, tags in neighbours:
        for t in tags:
            if t not in current_tags:
                tag_score[t] += score
    # Sort by total score desc, return top_k
    ranked = sorted(tag_score.items(), key=lambda kv: -kv[1])
    return [t for t, _ in ranked[:top_k]]


# ---- Main ---------------------------------------------------------

def main() -> int:
    # Pass 1: parse every file; build the candidate corpus for tag
    # suggestion.
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
            tags = parse_tags(parsed["frontmatter"].get("tags", ""))
            entries.append({
                "path": path,
                "subdir": subdir,
                "parsed": parsed,
                "tags": tags,
                "tokens": tokens(parsed["frontmatter"].get("title", "")
                                  + " " + parsed["body"][:1500]),
            })

    # Sparse-tag threshold: median per-entry tag count across the
    # corpus, minus 1. Entries with < that get flagged.
    tag_counts = [len(e["tags"]) for e in entries]
    tag_counts.sort()
    median_tag_count = tag_counts[len(tag_counts) // 2] if tag_counts else 0
    sparse_threshold = max(2, median_tag_count - 1)

    print(f"corpus size: {len(entries)} entries", file=sys.stderr)
    print(f"median tag count: {median_tag_count} (threshold: < {sparse_threshold})", file=sys.stderr)

    # Pass 2: detect opportunities per entry.
    rows = []
    empty_title_count = 0
    sparse_tags_count = 0
    for i, e in enumerate(entries):
        path = e["path"]
        parsed = e["parsed"]
        body = parsed["body"]
        rel_path = str(path.relative_to(VAULT_ROOT.parent))
        # Opportunity 1: empty title
        title_content = extract_title_section_content(body)
        needs_title = not bool(title_content)
        proposed_title = ""
        proposed_title_source = ""
        if needs_title:
            proposed_title, proposed_title_source = proposed_title_for_empty_section(parsed, path)
            empty_title_count += 1

        # Opportunity 2: sparse tags
        needs_more_tags = len(e["tags"]) < sparse_threshold
        proposed_additional_tags: list[str] = []
        if needs_more_tags:
            # Build candidates from the same subdir + nearby subdirs
            # to keep suggestions topically scoped.
            candidates = [{
                "tokens": other["tokens"],
                "tags": other["tags"],
            } for j, other in enumerate(entries) if j != i]
            proposed_additional_tags = suggest_tags_from_neighbours(
                e["tokens"], e["tags"], candidates,
            )
            sparse_tags_count += 1

        # Only surface rows where at least one opportunity exists
        if needs_title or needs_more_tags:
            rows.append({
                "path": rel_path,
                "subdir": e["subdir"],
                "filename": path.name,
                "frontmatter_title": parsed["frontmatter"].get("title", ""),
                "current_tags": e["tags"],
                "tag_count": len(e["tags"]),
                "needs_title": needs_title,
                "proposed_title": proposed_title,
                "proposed_title_source": proposed_title_source,
                "needs_more_tags": needs_more_tags,
                "proposed_additional_tags": proposed_additional_tags,
            })

    OUT_PATH.write_text("\n".join(json.dumps(r) for r in rows) + "\n")
    print(f"wrote {OUT_PATH} ({len(rows)} rows)", file=sys.stderr)
    print(f"  empty-title opportunities: {empty_title_count}", file=sys.stderr)
    print(f"  sparse-tag opportunities: {sparse_tags_count}", file=sys.stderr)

    # Per-subdir summary
    by_subdir = Counter(r["subdir"] for r in rows)
    print("\nopportunities by subdir:", file=sys.stderr)
    for sd in IN_SCOPE_SUBDIRS:
        total_in_subdir = sum(1 for e in entries if e["subdir"] == sd)
        print(f"  {sd}: {by_subdir.get(sd, 0)} of {total_in_subdir}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
