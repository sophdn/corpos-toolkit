#!/usr/bin/env python3
"""
T6 vault improvement — apply title fixes.

Reads measure/vault-improvement-corpus.jsonl, finds rows with
`needs_title: true` and a non-empty proposed_title, rewrites
each file by populating the empty `## Title` section with the
proposed title.

The empty-title shape:
    ## Title
    \n              ← (one or more blank lines)
    ## Body

After apply:
    ## Title
    \n
    <proposed_title>
    \n
    ## Body

Optional --source filter limits to a specific source kind:
  --source body_h1     (Batch 1: 72 entries)
  --source frontmatter (Batch 2: 2 entries)
  --source slug        (Batch 3: 2 entries)

Default: applies ALL non-empty proposals.

Optional --subdir filter limits to a specific subdir for per-
subdir commit grouping. Default: all subdirs.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
CORPUS_PATH = Path(__file__).resolve().parent / "vault-improvement-corpus.jsonl"

# The empty `## Title` section pattern — `## Title` then whitespace
# then `## Body`. Captures the surrounding whitespace so we can
# replace the entire span with the populated form.
EMPTY_TITLE_BODY_RE = re.compile(
    r"^(## Title\s*\n)\s*\n(## Body\b)",
    re.MULTILINE,
)


def apply_title(content: str, title: str) -> tuple[str, bool]:
    """Return (new_content, did_change). Returns (content, False)
    when the empty-title pattern isn't present (defensive — the
    surface script may have flagged a non-canonical-shape entry)."""
    def replace(m: re.Match) -> str:
        return m.group(1) + "\n" + title + "\n\n" + m.group(2)
    new_content, n = EMPTY_TITLE_BODY_RE.subn(replace, content, count=1)
    return (new_content, n > 0)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", choices=["body_h1", "frontmatter", "slug"],
                        help="filter to one title-source kind")
    parser.add_argument("--subdir", help="filter to one subdir")
    parser.add_argument("--dry-run", action="store_true",
                        help="print what would change, but don't write")
    args = parser.parse_args()

    rows = [json.loads(l) for l in CORPUS_PATH.open() if l.strip()]
    rows = [r for r in rows if r.get("needs_title") and r.get("proposed_title")]
    if args.source:
        rows = [r for r in rows if r.get("proposed_title_source") == args.source]
    if args.subdir:
        rows = [r for r in rows if r.get("subdir") == args.subdir]

    applied = 0
    skipped = 0
    for r in rows:
        full_path = VAULT_ROOT.parent / r["path"]
        if not full_path.exists():
            print(f"WARN: file missing: {full_path}", file=sys.stderr)
            continue
        original = full_path.read_text(encoding="utf-8")
        new_content, did_change = apply_title(original, r["proposed_title"])
        if not did_change:
            print(f"SKIP (no empty-title pattern): {r['filename']}", file=sys.stderr)
            skipped += 1
            continue
        if args.dry_run:
            print(f"would update: {r['filename']} ← {r['proposed_title'][:60]}")
            applied += 1
            continue
        full_path.write_text(new_content, encoding="utf-8")
        applied += 1

    print(f"\ntotal: applied={applied} skipped={skipped} eligible={len(rows)}",
          file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
