#!/usr/bin/env python3
"""
T6 vault linkage-apply — add wikilinks to a `## Related` section.

Reads measure/vault-linkage-corpus.jsonl. For each entry with
proposed links matching the requested tier(s), inserts the
wikilinks into a `## Related` section (creating one if absent,
extending if present).

Tiers:
  --tier auto         — Jaccard >= 0.35 (default; per
                        2026-05-21 conservative-auto policy)
  --tier review       — 0.20 <= Jaccard < 0.35 (operator-approved)
  --tier all          — both

Format inserted per link:
    - [[<target_slug>]] — <context>

Section placement:
- If `## Related` section already exists, APPEND new links
  (deduplicated against existing [[wikilinks]] in that section).
- If no `## Related` exists, APPEND a new section at end of file,
  ABOVE the trailing whitespace and ABOVE any frontmatter that
  sits at end (rare).

Idempotent: re-running doesn't double-add links that already
exist as [[wikilinks]] anywhere in the body.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
CORPUS_PATH = Path(__file__).resolve().parent / "vault-linkage-corpus.jsonl"

RELATED_SECTION_RE = re.compile(
    r"^## Related\s*\n(.*?)(?=^## |\Z)",
    re.MULTILINE | re.DOTALL,
)
WIKILINK_IN_BODY_RE = re.compile(r"\[\[([^\]]+)\]\]")


def render_link(target_slug: str, context: str) -> str:
    """Format one related-link bullet line."""
    return f"- [[{target_slug}]] — {context}"


def filter_links_by_tier(proposed: list[dict], tier: str) -> list[dict]:
    if tier == "auto":
        return [l for l in proposed if l["tier"] == "auto"]
    if tier == "review":
        return [l for l in proposed if l["tier"] == "review"]
    return list(proposed)


def insert_related_links(content: str, links: list[dict]) -> tuple[str, bool, int]:
    """Insert wikilinks into the file's `## Related` section,
    creating one if absent. Returns (new_content, did_change,
    inserted_count). Dedupes against any [[wikilinks]] already
    present in the body."""
    existing_slugs = set(WIKILINK_IN_BODY_RE.findall(content))
    fresh = [l for l in links if l["target_slug"] not in existing_slugs]
    if not fresh:
        return (content, False, 0)
    new_bullets = "\n".join(render_link(l["target_slug"], l["context"]) for l in fresh)
    # Existing Related section?
    m = RELATED_SECTION_RE.search(content)
    if m:
        section_body = m.group(1).rstrip()
        # Append our bullets after the existing content
        new_section = "## Related\n" + section_body + "\n" + new_bullets + "\n\n"
        new_content = content[:m.start()] + new_section + content[m.end():]
    else:
        # New section appended at end. Ensure trailing newline.
        suffix = "" if content.endswith("\n") else "\n"
        new_content = content + suffix + "\n## Related\n\n" + new_bullets + "\n"
    return (new_content, True, len(fresh))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tier", choices=["auto", "review", "all"],
                        default="auto",
                        help="which similarity tier to apply (default: auto)")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    rows = [json.loads(l) for l in CORPUS_PATH.open() if l.strip()]

    applied_files = 0
    total_inserted = 0
    for r in rows:
        filtered = filter_links_by_tier(r["proposed_links"], args.tier)
        if not filtered:
            continue
        full_path = VAULT_ROOT.parent / r["path"]
        if not full_path.exists():
            print(f"WARN: missing file: {full_path}", file=sys.stderr)
            continue
        original = full_path.read_text(encoding="utf-8")
        new_content, did_change, n_new = insert_related_links(original, filtered)
        if not did_change:
            print(f"SKIP (all links already present): {r['filename']}", file=sys.stderr)
            continue
        total_inserted += n_new
        if args.dry_run:
            print(f"would update {r['filename']} (+{n_new} links)")
            applied_files += 1
            continue
        full_path.write_text(new_content, encoding="utf-8")
        applied_files += 1
        print(f"applied {r['filename']} (+{n_new} links)")

    print(f"\ntotal: files={applied_files} links_inserted={total_inserted} tier={args.tier}",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
