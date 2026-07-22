#!/usr/bin/env python3
"""Backfill knowledge_pointers rows for vault files authored via Write-without-forge.

Resolves bug `vault-write-without-forge-110-files-unindexed-in-knowledge-pointers`
(id 1478). T1 of chain vault-hygiene-extract-and-migrate measured ~110 .md files
under ~/.claude/vault/ with no row in knowledge_pointers — Write-tool authoring
bypasses the indexsync hook (forge/indexsync.go::buildVaultNotePointer) that
forge(vault-note) fires on create.

Strategy (bug's option (a)): re-forge each unindexed entry via
forge_edit(vault-note). forge_edit re-fires indexsync; if the entry's
knowledge_pointer is missing, the call re-creates it. forge_edit is idempotent —
re-running the script is safe.

Constraints:
  - Only operates on files under vault/decisions/, vault/learnings/<project>/,
    and vault/reference/ — the three subdirs vault-note routes to. Files under
    vault/projects/, vault/meta/, vault/scratch/ are intentionally
    skipped (they're authored via Write per skill convention).
  - Frontmatter must have `slug:` to call forge_edit. Files without a slug get
    flagged but not modified.
  - Toolkit-server HTTP daemon must be reachable on $TOOLKIT_HTTP_PORT
    (defaults to 3000).

Run:  python3 scripts/backfill-vault-knowledge-pointers.py [--dry-run]
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
import urllib.error
import urllib.request
from pathlib import Path

VAULT_ROOT = Path.home() / ".claude" / "vault"
ROUTED_SUBDIRS = ("decisions", "learnings", "reference")
DB_PATH = Path(
    os.environ.get("TOOLKIT_DB", str(Path.home() / "dev" / "mcp-servers" / "data" / "toolkit.db"))
)
MCP_URL = f"http://127.0.0.1:{os.environ.get('TOOLKIT_HTTP_PORT', '3000')}/mcp/work"


def parse_frontmatter(text: str) -> dict[str, str]:
    """Extract the YAML frontmatter from a vault file. Returns {} if missing or
    malformed. Doesn't depend on PyYAML — vault frontmatter is flat key:value
    so a hand-rolled parser is sufficient and avoids the install dep."""
    if not text.startswith("---"):
        return {}
    end = text.find("\n---", 4)
    if end < 0:
        return {}
    out: dict[str, str] = {}
    for line in text[4:end].splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        if ":" not in line:
            continue
        key, _, value = line.partition(":")
        out[key.strip()] = value.strip().strip("\"'")
    return out


def collect_routed_files() -> list[Path]:
    """All vault files under the three routed subdirs."""
    files: list[Path] = []
    for subdir in ROUTED_SUBDIRS:
        root = VAULT_ROOT / subdir
        if not root.exists():
            continue
        for path in root.rglob("*.md"):
            if path.is_file():
                files.append(path)
    return files


def indexed_paths() -> set[str]:
    """Set of source_ref values already present in knowledge_pointers for vault."""
    if not DB_PATH.exists():
        sys.stderr.write(f"ERROR: db missing at {DB_PATH}\n")
        sys.exit(2)
    con = sqlite3.connect(str(DB_PATH))
    try:
        rows = con.execute(
            "SELECT source_ref FROM knowledge_pointers WHERE source_type = 'vault'"
        ).fetchall()
    finally:
        con.close()
    return {row[0] for row in rows}


def derive_kind(path: Path) -> str | None:
    """Map vault subdir to note_kind."""
    try:
        rel = path.relative_to(VAULT_ROOT)
    except ValueError:
        return None
    first = rel.parts[0] if rel.parts else ""
    if first == "decisions":
        return "decision"
    if first == "reference":
        return "reference"
    if first == "learnings":
        return "learning"
    return None


def derive_scope(path: Path, kind: str) -> str:
    """For learnings, the routing scope is the directory name under learnings/.
    'general' means cross-project — represented as empty scope. Decisions and
    reference notes don't carry a scope (they're cross-project by convention
    and route to top-level decisions/ + reference/ regardless).

    Post-rename of forge(vault-note) schema field `project` → `scope` (chain
    `forge-vault-note-schema-rework`, T3); the prior top-level-`project`
    routing path is gone — only the schema field controls routing now.
    """
    if kind != "learning":
        return ""
    try:
        rel = path.relative_to(VAULT_ROOT / "learnings")
    except ValueError:
        return ""
    if not rel.parts or rel.parts[0] == "general":
        return ""
    return rel.parts[0]


def call_forge_edit(slug: str, kind: str, scope: str, title: str, body: str, tags: str) -> tuple[bool, str]:
    """POST forge_edit to the HTTP MCP surface. Returns (ok, message).

    Top-level `project` is omitted for vault-note (cross-project schema; the
    dispatcher's project-required gate is exempt via [schema].cross_project).
    Routing is driven solely by the `scope` field inside `fields`.
    """
    fields = {"title": title, "body": body, "tags": tags, "note_kind": kind}
    if scope:
        fields["scope"] = scope
    payload = {
        "action": "forge_edit",
        "rationale": "vault-knowledge-pointers backfill (bug 1478)",
        "params": {"schema_name": "vault-note", "slug": slug, "fields": fields},
    }
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(MCP_URL, data=data, headers={"content-type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            body_str = resp.read().decode("utf-8")
    except urllib.error.URLError as e:
        return False, f"http error: {e}"
    try:
        envelope = json.loads(body_str)
    except json.JSONDecodeError:
        return False, f"non-json response: {body_str[:160]}"
    if "content" in envelope:
        inner_text = envelope["content"][0].get("text", "")
        try:
            inner = json.loads(inner_text)
        except json.JSONDecodeError:
            inner = {"raw": inner_text}
    else:
        inner = envelope
    if isinstance(inner, dict) and inner.get("ok") is False:
        return False, inner.get("error") or inner.get("message") or "forge_edit returned ok=false"
    return True, "ok"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true", help="report planned actions; no mutation")
    parser.add_argument("--limit", type=int, default=0, help="stop after N forge_edit calls (debug)")
    args = parser.parse_args()

    files = collect_routed_files()
    indexed = indexed_paths()
    # knowledge_pointers.source_ref is the path relative to ~/.claude/vault/
    # (e.g. "decisions/2026-05-05_foo.md"). Compare against that shape.
    unindexed = [p for p in files if str(p.relative_to(VAULT_ROOT)) not in indexed]

    print(f"vault root:        {VAULT_ROOT}")
    print(f"toolkit-server:    {MCP_URL}")
    print(f"db:                {DB_PATH}")
    print(f"routed-subdir .md: {len(files)}")
    print(f"indexed vault rows: {len(indexed)}")
    print(f"unindexed (target): {len(unindexed)}")
    print()

    if not unindexed:
        print("nothing to backfill.")
        return 0
    if args.dry_run:
        print("--- DRY RUN (no calls will fire) ---")

    failures: list[tuple[Path, str]] = []
    skipped: list[tuple[Path, str]] = []
    ok = 0

    for i, path in enumerate(unindexed, 1):
        if args.limit and ok >= args.limit:
            print(f"hit --limit {args.limit}; stopping early")
            break
        kind = derive_kind(path)
        if kind is None:
            skipped.append((path, "unmapped subdir"))
            continue
        scope = derive_scope(path, kind)
        text = path.read_text(encoding="utf-8", errors="replace")
        fm = parse_frontmatter(text)
        slug = fm.get("slug") or ""
        if not slug:
            # Fall back to filename-derived slug (strip date prefix + .md).
            stem = path.stem
            if len(stem) > 11 and stem[:10].count("-") == 2 and stem[10] == "_":
                slug = stem[11:]
            else:
                slug = stem
        title = fm.get("title") or slug.replace("-", " ")
        tags = fm.get("tags") or ""
        if tags.startswith("[") and tags.endswith("]"):
            tags = tags[1:-1].replace(", ", ",").replace(" ", ",")
        # Body is the content AFTER the frontmatter.
        if text.startswith("---"):
            end = text.find("\n---", 4)
            body = text[end + 4 :].lstrip("\n") if end >= 0 else text
        else:
            body = text

        if args.dry_run:
            print(f"[{i:3}/{len(unindexed)}] DRY {kind:<9} scope={scope or '-':<14} slug={slug}")
            ok += 1
            continue

        success, msg = call_forge_edit(slug, kind, scope, title, body, tags)
        if success:
            print(f"[{i:3}/{len(unindexed)}] OK  {kind:<9} {path.relative_to(VAULT_ROOT)}")
            ok += 1
        else:
            print(f"[{i:3}/{len(unindexed)}] ERR {kind:<9} {path.relative_to(VAULT_ROOT)}: {msg}")
            failures.append((path, msg))

    print()
    print(f"summary: {ok} indexed, {len(failures)} failed, {len(skipped)} skipped")
    if skipped:
        print()
        print("skipped:")
        for path, reason in skipped:
            print(f"  {path}  ({reason})")
    if failures:
        print()
        print("failures:")
        for path, reason in failures:
            print(f"  {path}  ({reason})")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
