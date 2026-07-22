#!/usr/bin/env python3
"""
T6 vault improvement — apply sparse-tag fixes.

Reads measure/vault-improvement-corpus.jsonl, finds rows with
`needs_more_tags: true`, FILTERS the proposed tags by filename/
title overlap (the neighbour-based suggester produces noisy tags
from token-similar but topically-unrelated entries), and applies
only the filtered set.

Filter rule per 2026-05-21 user spot-check finding:
A proposed tag passes only if at least one of:
  (a) the tag (or its `-`-stripped form) appears as a token in
      the filename, OR
  (b) the tag appears as a substring in the frontmatter title.

This drops neighbour-pollution (rubric chain entries getting
"dispatch / mcp / qwen-offload" tags from peers).

Conservative: applies ONLY to entries with current_tags == []
(zero existing tags). For entries with some tags already, the
duplicate-detection and existing-tag interplay needs operator
judgment; defer those to a manual pass.

Cap per entry: 3 newly-added tags max. Avoids over-tagging.

Default: --dry-run NOT passed → applies live mutations.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
CORPUS_PATH = Path(__file__).resolve().parent / "vault-improvement-corpus.jsonl"

MAX_TAGS_PER_ENTRY = 3

# Frontmatter parser (reuses the surface-script shape).
FRONTMATTER_RE = re.compile(r"^---\n(.*?)\n---\n", re.DOTALL)


def filename_tokens(filename: str) -> set[str]:
    """Lowercase tokens from the filename (drops .md, drops date
    prefix). Used to verify a proposed tag is topically relevant."""
    stem = filename.removesuffix(".md")
    if stem.startswith("20") and "_" in stem:
        stem = stem.split("_", 1)[1]
    parts = re.split(r"[-_]", stem.lower())
    return {p for p in parts if len(p) >= 3}


def title_substring_match(tag: str, title: str) -> bool:
    """Check if the tag appears as a substring in the title
    (case-insensitive). Handles hyphenated tags by also checking
    the space-replaced form."""
    if not title:
        return False
    title_l = title.lower()
    tag_l = tag.lower()
    return tag_l in title_l or tag_l.replace("-", " ") in title_l


def filter_proposed_tags(
    proposed: list[str],
    filename: str,
    title: str,
) -> list[str]:
    """Return only proposed tags that pass the topical-overlap
    filter. Limits to MAX_TAGS_PER_ENTRY."""
    fn_tokens = filename_tokens(filename)
    kept: list[str] = []
    for tag in proposed:
        # Variant set: the tag itself, plus path-segment splits
        # (e.g., "project/mcp-servers" → {"project", "mcp-servers", "mcp"})
        variants = {tag}
        if "/" in tag:
            variants.update(tag.split("/"))
        for v in tag.lower().split("-"):
            if len(v) >= 3:
                variants.add(v)
        # Pass condition: any variant appears in filename tokens
        # OR the full tag appears as substring of title.
        passes = any(v.lower() in fn_tokens for v in variants) or title_substring_match(tag, title)
        if passes:
            kept.append(tag)
        if len(kept) >= MAX_TAGS_PER_ENTRY:
            break
    return kept


def apply_tags_to_file(path: Path, new_tags: list[str]) -> bool:
    """Insert / extend the frontmatter `tags:` field. Returns True
    if the file was changed. Conservative: only operates on files
    whose frontmatter has no existing tags field OR has an empty
    array. Refuses to mutate non-empty tags fields."""
    text = path.read_text(encoding="utf-8")
    m = FRONTMATTER_RE.match(text)
    if not m:
        print(f"WARN no frontmatter: {path.name}", file=sys.stderr)
        return False
    fm_raw = m.group(1)
    fm_end = m.end()
    # Check existing tags field
    tags_line_re = re.compile(r"^tags:\s*(.*)$", re.MULTILINE)
    tm = tags_line_re.search(fm_raw)
    new_tags_str = "[" + ", ".join(new_tags) + "]"
    if tm:
        existing = tm.group(1).strip()
        if existing and existing not in ("[]", "{}"):
            print(f"WARN existing tags field non-empty, skip: {path.name}", file=sys.stderr)
            return False
        # Replace empty tags field
        new_fm_raw = fm_raw[:tm.start()] + f"tags: {new_tags_str}" + fm_raw[tm.end():]
    else:
        # Append new tags line before frontmatter end
        new_fm_raw = fm_raw.rstrip() + f"\ntags: {new_tags_str}"
    new_text = "---\n" + new_fm_raw + "\n---\n" + text[fm_end:]
    path.write_text(new_text, encoding="utf-8")
    return True


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    rows = [json.loads(l) for l in CORPUS_PATH.open() if l.strip()]
    # Conservative scope: only entries with empty current_tags +
    # at least one proposed tag.
    rows = [
        r for r in rows
        if r.get("needs_more_tags")
        and not r.get("current_tags")
        and r.get("proposed_additional_tags")
    ]

    applied = 0
    filtered_out = 0
    no_overlap = 0
    for r in rows:
        full_path = VAULT_ROOT.parent / r["path"]
        if not full_path.exists():
            print(f"WARN: missing file: {full_path}", file=sys.stderr)
            continue
        kept = filter_proposed_tags(
            r["proposed_additional_tags"],
            r["filename"],
            r.get("frontmatter_title", ""),
        )
        if not kept:
            print(f"DROP (no topical overlap): {r['filename']}", file=sys.stderr)
            no_overlap += 1
            continue
        filtered_out += len(r["proposed_additional_tags"]) - len(kept)
        if args.dry_run:
            print(f"would tag: {r['filename']} ← {kept}")
            applied += 1
            continue
        if apply_tags_to_file(full_path, kept):
            applied += 1
            print(f"applied: {r['filename']} ← {kept}")

    print(f"\ntotal: applied={applied} no-overlap-dropped={no_overlap} tags-filtered-out={filtered_out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
