#!/usr/bin/env python3
"""Migrate existing harness-default auto-memory entries into the vault.

One-shot script for chain `memory-substrate-within-vault` T4. Walks every
~/.claude/projects/<project-dir>/memory/<name>.md file, parses
frontmatter, and re-forges each entry via the new `memory` schema
introduced in T2. Emits a MemoryWritten event per re-forge (with
source="migration") so the migration is auditable via the events
ledger.

Idempotent: re-running after a partial failure skips entries whose
target vault path already exists. Never deletes the original
harness-dir files — T3's session-init materialization hook handles
cleanup on the next session start.

Frontmatter variance tolerance: existing entries carry inconsistent
shapes (some with metadata.type, some with top-level type, some with
neither). Kind extraction preference order:
  1. frontmatter `metadata.type:`
  2. frontmatter top-level `type:`
  3. filename prefix (feedback_*, project_*, reference_*, user_*)
  4. reject + surface in the report

Filename-vs-frontmatter name disambiguation: the canonical `name` for
the vault entry is the frontmatter `name:` field; the filename
contributes only as a fallback when frontmatter `name:` is absent or
non-kebab-case.

Usage:
  scripts/migrate-memories-to-vault.py --dry-run   # report only
  scripts/migrate-memories-to-vault.py --execute   # actually migrate
"""
from __future__ import annotations

import argparse
import json
import re
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path

KEBAB_RE = re.compile(r"^[a-z0-9-]+$")
KIND_VALUES = {"user", "feedback", "project", "reference"}
KIND_FILENAME_PREFIX = {
    "feedback_": "feedback",
    "feedback-": "feedback",
    "project_": "project",
    "project-": "project",
    "reference_": "reference",
    "reference-": "reference",
    "user_": "user",
    "user-": "user",
}


@dataclass
class MemoryEntry:
    source_path: Path
    project_dir_slug: str  # e.g. "-home-sophi-dev-mcp-servers"
    project_id: str        # e.g. "mcp-servers" (resolved from suffix)
    kind: str
    name: str
    description: str
    body: str


def parse_frontmatter(text: str) -> tuple[dict, str]:
    """Crude YAML frontmatter parser: handles the auto-memory shapes
    we see in the source set. Not a general YAML parser — but the
    auto-memory contract is narrow: `key: value` lines plus a nested
    `metadata:` block with `  key: value` lines."""
    if not text.startswith("---\n"):
        return {}, text
    end = text.find("\n---\n", 4)
    if end == -1:
        return {}, text
    fm_text = text[4:end]
    body = text[end + 5:]

    fm: dict = {}
    metadata: dict = {}
    in_metadata = False
    for line in fm_text.splitlines():
        if not line.strip():
            continue
        if line.startswith("metadata:"):
            in_metadata = True
            continue
        if in_metadata and line.startswith("  "):
            kv = line.lstrip()
            if ":" in kv:
                k, _, v = kv.partition(":")
                metadata[k.strip()] = v.strip().strip('"').strip("'")
            continue
        if line.startswith(" "):
            continue
        in_metadata = False
        if ":" in line:
            k, _, v = line.partition(":")
            fm[k.strip()] = v.strip().strip('"').strip("'")
    if metadata:
        fm["metadata"] = metadata
    return fm, body.lstrip("\n")


def resolve_project_id(project_dir_slug: str, registered_ids: set[str]) -> str:
    """Derive a project_id from the harness dir slug. Convention: the
    dir slug is the launch dir with / → -. Match the dir slug's suffix
    against registered project_ids; on a tie, prefer the longest match.

    Edge case: -home-sophi-dev (parent dev dir) and -home-sophi (global
    home) don't match a specific registered project. Fall back to
    `mcp-servers` as the canonical home — those entries were authored
    in agent sessions whose launch dir was the dev parent; tagging
    them with the current canonical home is least bad. T7's
    retrospective can call out the structural limitation that
    feedback/project/reference kinds are project-scoped while these
    entries may apply across projects.

    NEVER fall back to the daemon's --default-project sentinel
    (currently seed-packet on this machine) — that misroutes 15+
    entries authored from non-seed-packet contexts.
    """
    base = project_dir_slug.lstrip("-")
    matches = sorted(
        (pid for pid in registered_ids if base.endswith(pid)),
        key=len,
        reverse=True,
    )
    if matches:
        return matches[0]
    return "mcp-servers"


def kebabify(name: str) -> str:
    """Convert snake_case or mixed to kebab-case. Strip leading kind_
    prefix if present (e.g. feedback_foo_bar → foo-bar)."""
    out = name.replace("_", "-").lower()
    for prefix in ("feedback-", "project-", "reference-", "user-"):
        if out.startswith(prefix):
            out = out[len(prefix):]
            break
    return out


def derive_entry(path: Path, registered_ids: set[str]) -> tuple[MemoryEntry | None, str | None]:
    """Parse one harness memory file into a MemoryEntry. Returns
    (entry, None) on success or (None, error_msg) on parse failure."""
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as e:
        return None, f"read failed: {e}"

    fm, body = parse_frontmatter(text)
    if not fm:
        return None, "no frontmatter"

    # Kind extraction: metadata.type → top-level type → filename prefix.
    kind = ""
    if isinstance(fm.get("metadata"), dict):
        kind = fm["metadata"].get("type", "")
    if not kind:
        kind = fm.get("type", "")
    if not kind:
        stem = path.stem
        for prefix, mapped in KIND_FILENAME_PREFIX.items():
            if stem.startswith(prefix):
                kind = mapped
                break
    if kind not in KIND_VALUES:
        return None, f"unrecognized kind {kind!r}"

    # Name extraction: frontmatter name: → filename (kebabified).
    name = fm.get("name", "").strip()
    if not name or not KEBAB_RE.match(name):
        name = kebabify(path.stem)
    if not KEBAB_RE.match(name):
        return None, f"derived name {name!r} not kebab-case"

    description = fm.get("description", "").strip()
    if not description:
        description = f"(migrated entry; original frontmatter had no description)"

    body = body.strip()
    if not body:
        return None, "empty body"

    project_dir_slug = path.parts[-3]  # ".claude/projects/<slug>/memory/<file>"
    project_id = resolve_project_id(project_dir_slug, registered_ids)

    return MemoryEntry(
        source_path=path,
        project_dir_slug=project_dir_slug,
        project_id=project_id,
        kind=kind,
        name=name,
        description=description,
        body=body,
    ), None


def vault_target_path(vault_root: Path, entry: MemoryEntry) -> Path:
    return vault_root / "memory" / entry.kind / f"{entry.name}.md"


def post_forge(mcp_url: str, entry: MemoryEntry) -> tuple[bool, str]:
    """Call forge(schema_name=memory, ...) via the HTTP MCP surface."""
    fields = {
        "memory_kind": entry.kind,
        "name": entry.name,
        "description": entry.description,
        "body": entry.body,
        "source": "migration",
    }
    payload = {
        "action": "forge",
        "rationale": f"migrate {entry.source_path} from harness-default dir into vault",
        "params": {
            "schema_name": "memory",
            "slug": entry.name,
            "fields": fields,
        },
    }
    if entry.project_id:
        payload["project"] = entry.project_id
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        mcp_url,
        data=data,
        headers={"content-type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            body = resp.read().decode("utf-8", "replace")
            if resp.status >= 400:
                return False, f"http {resp.status}: {body[:200]}"
            try:
                parsed = json.loads(body)
            except json.JSONDecodeError:
                return False, f"non-json response: {body[:200]}"
            if isinstance(parsed, dict) and parsed.get("error"):
                return False, f"forge error: {parsed['error']}"
            return True, ""
    except urllib.error.URLError as e:
        return False, f"connect failed: {e}"
    except OSError as e:
        return False, f"transport error: {e}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--dry-run", action="store_true", help="report only; no writes")
    parser.add_argument("--execute", action="store_true", help="perform the migration")
    parser.add_argument("--projects-root", default=str(Path.home() / ".claude" / "projects"))
    parser.add_argument("--vault-root", default=str(Path.home() / ".claude" / "vault"))
    parser.add_argument("--mcp-url", default="http://127.0.0.1:3000/mcp/work")
    parser.add_argument("--registered-projects", nargs="*", default=[
        "dm-toolkit", "hermes", "lab-app", "mcp-servers",
        "seed-packet", "self-compile", "voice-trainer",
        "mempalace-rust",  # not registered today but exists as a memory dir
    ])
    args = parser.parse_args()

    if args.dry_run == args.execute:
        parser.error("exactly one of --dry-run / --execute is required")

    projects_root = Path(args.projects_root)
    vault_root = Path(args.vault_root)
    registered_ids = set(args.registered_projects)

    if not projects_root.is_dir():
        print(f"ERROR: projects root not found: {projects_root}", file=sys.stderr)
        return 2

    # Walk every project's memory dir.
    sources = sorted(projects_root.glob("*/memory/*.md"))
    sources = [p for p in sources if p.name != "MEMORY.md"]

    print(f"[migrate] found {len(sources)} candidate entries across "
          f"{len({p.parts[-3] for p in sources})} project dirs")
    print(f"[migrate] mode: {'DRY-RUN' if args.dry_run else 'EXECUTE'}")
    print()

    migrated = 0
    skipped_existing = 0
    rejected = 0
    rejected_reasons: list[str] = []

    for path in sources:
        entry, err = derive_entry(path, registered_ids)
        if err is not None:
            rejected += 1
            rejected_reasons.append(f"  {path}: {err}")
            continue
        target = vault_target_path(vault_root, entry)
        if target.exists():
            skipped_existing += 1
            continue

        if args.dry_run:
            print(f"  WOULD migrate {path.relative_to(projects_root)} → "
                  f"vault/memory/{entry.kind}/{entry.name}.md "
                  f"(project_id={entry.project_id or '<none>'})")
            migrated += 1
            continue

        ok, msg = post_forge(args.mcp_url, entry)
        if ok:
            migrated += 1
            print(f"  OK {entry.name} ({entry.kind}) ← {path.parts[-3]}")
        else:
            rejected += 1
            rejected_reasons.append(f"  {path}: forge failed: {msg}")

    print()
    print(f"[migrate] migrated={migrated} skipped_existing={skipped_existing} rejected={rejected}")
    if rejected_reasons:
        print()
        print("[migrate] rejected entries:")
        for line in rejected_reasons:
            print(line)
    return 0 if rejected == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
