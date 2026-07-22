#!/usr/bin/env python3
"""
spike_label_coverage.py — TT1.5 spike: hand-label coverage helper.

Walks every JSONL transcript under ~/.claude/projects/*/ and groups
records into spans keyed by promptId. For each span, emits a summary
containing: every search-tool call (vault_search, kiwix_search,
knowledge_search, or their legacy mcp__work-server__* / mcp__toolkit-server__*
equivalents), every returned source_ref, every subsequent vault_read /
kiwix_fetch / Read on those source_refs, every subsequent assistant
text snippet that may quote/mention them, and a terminal-event flag
(was the span closed by a bug_resolve / task_complete / chain_close?).

The output is intentionally a digest, not full transcripts — the goal
is to give a hand-labeler enough context per span to assign click_kind
values to (span, source_ref) pairs without re-reading the raw JSONL.

Run modes:
  inventory  — print a per-project span count summary.
  sample     — write a sampled span set (~30-50) to a digest JSON file.
  summarize  — given a sampled span, print its human-readable digest.

Output location: defaults to ./tt15_spike/ in the current directory.
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Iterable

PROJECTS_DIR = Path.home() / ".claude" / "projects"
OUT_DIR_DEFAULT = Path.cwd() / "tt15_spike"

# Search tool detection — across the union of naming conventions seen in
# the transcript history (legacy mcp__work-server__* separate-tool form
# and unified mcp__toolkit-server__work/knowledge with action= form).
SEARCH_TOOLS_UNIFIED = {
    ("mcp__toolkit-server__knowledge", "vault_search"),
    ("mcp__toolkit-server__knowledge", "kiwix_search"),
    ("mcp__toolkit-server__knowledge", "knowledge_search"),
    ("knowledge", "vault_search"),
    ("knowledge", "kiwix_search"),
    ("knowledge", "knowledge_search"),
}
SEARCH_TOOLS_LEGACY = {
    "mcp__work-server__vault_search",
    "mcp__work-server__kiwix_search",
    "mcp__work-server__knowledge_search",
}
FOLLOW_TOOLS_UNIFIED = {
    ("mcp__toolkit-server__knowledge", "vault_read"),
    ("mcp__toolkit-server__knowledge", "kiwix_fetch"),
    ("knowledge", "vault_read"),
    ("knowledge", "kiwix_fetch"),
}
FOLLOW_TOOLS_NATIVE = {"Read"}
RESOLVE_TOOLS_UNIFIED = {
    ("mcp__toolkit-server__work", "bug_resolve"),
    ("mcp__toolkit-server__work", "task_complete"),
    ("mcp__toolkit-server__work", "task_cancel"),
    ("mcp__toolkit-server__work", "chain_close"),
    ("work", "bug_resolve"),
    ("work", "task_complete"),
    ("work", "task_cancel"),
    ("work", "chain_close"),
}
RESOLVE_TOOLS_LEGACY = {
    "mcp__work-server__bug_resolve",
    "mcp__work-server__complete_task",
    "mcp__work-server__cancel_task",
    "mcp__work-server__close_chain",
}


def classify_tool_use(name: str, action: str | None) -> str | None:
    """Return one of 'search' / 'follow' / 'resolve' / None."""
    if (name, action) in SEARCH_TOOLS_UNIFIED or name in SEARCH_TOOLS_LEGACY:
        return "search"
    if (name, action) in FOLLOW_TOOLS_UNIFIED or name in FOLLOW_TOOLS_NATIVE:
        return "follow"
    if (name, action) in RESOLVE_TOOLS_UNIFIED or name in RESOLVE_TOOLS_LEGACY:
        return "resolve"
    return None


def iter_jsonl(path: Path) -> Iterable[dict]:
    try:
        with path.open() as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    yield json.loads(line)
                except json.JSONDecodeError:
                    continue
    except OSError:
        return


def extract_text(content_block: Any) -> str:
    if isinstance(content_block, str):
        return content_block
    if isinstance(content_block, dict):
        if content_block.get("type") == "text":
            return content_block.get("text", "") or ""
        if content_block.get("type") == "tool_result":
            c = content_block.get("content")
            if isinstance(c, str):
                return c
            if isinstance(c, list):
                return "\n".join(extract_text(x) for x in c)
    return ""


def parse_source_refs_from_tool_result(text: str) -> list[str]:
    """Best-effort parse: search tool results usually return JSON with
    `source_ref` keys, or markdown with `path:` / `slug:` fields. Pull
    every plausible reference."""
    refs: list[str] = []
    # JSON-shaped: walk for source_ref / path / slug
    try:
        d = json.loads(text)
        def walk(o):
            if isinstance(o, dict):
                for k, v in o.items():
                    if k in ("source_ref", "path", "slug", "file") and isinstance(v, str):
                        refs.append(v)
                    walk(v)
            elif isinstance(o, list):
                for x in o:
                    walk(x)
        walk(d)
    except (json.JSONDecodeError, ValueError):
        pass
    # Fallback / supplement: regex over the text body for typical refs.
    import re
    for m in re.finditer(r'(?:source_ref|path|slug|file)["\']?\s*[:=]\s*["\']([^"\'\n]+)["\']', text):
        refs.append(m.group(1))
    # Vault path heuristic: vault/learnings/general/<date>_<slug>.md
    for m in re.finditer(r'(?:~/\.claude/)?vault/[a-z0-9_/-]+\.md', text):
        refs.append(m.group(0))
    # Dedup preserving order
    seen = set()
    out = []
    for r in refs:
        if r and r not in seen:
            seen.add(r)
            out.append(r)
    return out


def collect_spans(project_dir: Path) -> dict[str, dict]:
    """Per promptId, accumulate the span digest."""
    spans: dict[str, dict] = defaultdict(lambda: {
        "promptId": None,
        "sessionId": None,
        "project_slug": project_dir.name,
        "is_sidechain": False,
        "parent_uuid": None,
        "first_ts": None,
        "last_ts": None,
        "user_messages": [],
        "search_calls": [],     # list of {ts, name, action, query, source_refs, span_position}
        "follow_calls": [],     # list of {ts, name, target, span_position}
        "resolve_calls": [],    # list of {ts, name, action, entity_slug, kind, rationale, span_position}
        "assistant_text_snippets": [],  # list of (ts, text_excerpt)
        "record_count": 0,
    })

    for jsonl in sorted(project_dir.glob("*.jsonl")):
        # Map record uuid → tool_use type for matching tool_result back.
        # tool_results carry the tool_use_id; we look up to recover source_refs.
        tool_use_kinds: dict[str, dict] = {}
        # promptId travels on user records; subsequent assistant turns,
        # tool_use blocks, and tool_results inherit the most recent user
        # promptId in stream order. Reset on each new file.
        current_pid: str | None = None
        for record in iter_jsonl(jsonl):
            pid = record.get("promptId")
            if pid:
                current_pid = pid
            else:
                pid = current_pid
            if not pid:
                continue
            sp = spans[pid]
            sp["record_count"] += 1
            if sp["promptId"] is None:
                sp["promptId"] = pid
                sp["sessionId"] = record.get("sessionId")
                sp["is_sidechain"] = bool(record.get("isSidechain"))
                sp["parent_uuid"] = record.get("parentUuid")
                sp["first_ts"] = record.get("timestamp")
            sp["last_ts"] = record.get("timestamp")

            rtype = record.get("type")
            ts = record.get("timestamp")
            msg = record.get("message") or {}
            if not isinstance(msg, dict):
                continue
            content = msg.get("content")
            if rtype == "user" and isinstance(content, str):
                sp["user_messages"].append({"ts": ts, "text": content[:600]})
            if not isinstance(content, list):
                continue

            for block in content:
                if not isinstance(block, dict):
                    continue
                btype = block.get("type")
                if btype == "tool_use":
                    name = block.get("name", "")
                    inp = block.get("input") or {}
                    action = inp.get("action") if isinstance(inp, dict) else None
                    kind = classify_tool_use(name, action)
                    use_id = block.get("id")
                    if kind == "search":
                        params = inp.get("params") if isinstance(inp, dict) else None
                        # params occasionally arrive as a JSON string instead of a dict
                        if isinstance(params, str):
                            try:
                                params = json.loads(params)
                            except (json.JSONDecodeError, ValueError):
                                params = {}
                        if not isinstance(params, dict):
                            params = {}
                        src = params or (inp if isinstance(inp, dict) else {})
                        if not isinstance(src, dict):
                            src = {}
                        query = src.get("query") or src.get("pattern") or ""
                        if isinstance(query, str):
                            query = query[:200]
                        else:
                            query = str(query)[:200]
                        entry = {
                            "ts": ts, "name": name, "action": action,
                            "query": query,
                            "source_refs": [],
                            "tool_use_id": use_id,
                            "span_position": sp["record_count"],
                        }
                        sp["search_calls"].append(entry)
                        tool_use_kinds[use_id] = entry
                    elif kind == "follow":
                        target = ""
                        if isinstance(inp, dict):
                            p = inp.get("params") or {}
                            if isinstance(p, str):
                                try: p = json.loads(p)
                                except (json.JSONDecodeError, ValueError): p = {}
                            if not isinstance(p, dict): p = {}
                            target = (p.get("path") or p.get("file_path") or p.get("zim_id")
                                      or inp.get("file_path") or inp.get("path") or "")
                        sp["follow_calls"].append({
                            "ts": ts, "name": name, "target": str(target)[:300],
                            "span_position": sp["record_count"],
                        })
                    elif kind == "resolve":
                        p = inp.get("params") if isinstance(inp, dict) else inp
                        if isinstance(p, str):
                            try: p = json.loads(p)
                            except (json.JSONDecodeError, ValueError): p = {}
                        slug = ""
                        rationale = ""
                        rkind = ""
                        if isinstance(p, dict):
                            slug = p.get("slug") or p.get("entity_slug") or ""
                            rkind = p.get("kind") or ""
                            rationale = (p.get("resolution_note") or p.get("closure_summary")
                                         or p.get("reason") or p.get("rationale") or "")
                        sp["resolve_calls"].append({
                            "ts": ts, "name": name, "action": action,
                            "entity_slug": str(slug)[:120],
                            "kind": str(rkind)[:60],
                            "rationale": str(rationale)[:600],
                            "span_position": sp["record_count"],
                        })
                elif btype == "tool_result":
                    use_id = block.get("tool_use_id")
                    if use_id and use_id in tool_use_kinds:
                        text = extract_text(block)
                        refs = parse_source_refs_from_tool_result(text)
                        tool_use_kinds[use_id]["source_refs"] = refs
                elif btype == "text":
                    text = block.get("text", "") or ""
                    if text and msg.get("role") == "assistant":
                        sp["assistant_text_snippets"].append({"ts": ts, "text": text[:1200]})
    return spans


def inventory() -> None:
    print(f"PROJECTS_DIR = {PROJECTS_DIR}")
    grand_total = 0
    for proj in sorted(PROJECTS_DIR.iterdir()):
        if not proj.is_dir():
            continue
        spans = collect_spans(proj)
        n_spans = len(spans)
        n_with_search = sum(1 for s in spans.values() if s["search_calls"])
        n_with_resolve = sum(1 for s in spans.values() if s["resolve_calls"])
        n_sidechain = sum(1 for s in spans.values() if s["is_sidechain"])
        print(f"  {proj.name:40s}  spans={n_spans:5d}  with_search={n_with_search:4d}"
              f"  with_resolve={n_with_resolve:4d}  sidechain={n_sidechain:3d}")
        grand_total += n_spans
    print(f"  TOTAL spans={grand_total}")


def sample(out_dir: Path, target_n: int = 40, seed: int = 17) -> None:
    rng = random.Random(seed)
    all_spans: list[dict] = []
    for proj in sorted(PROJECTS_DIR.iterdir()):
        if not proj.is_dir():
            continue
        for span in collect_spans(proj).values():
            if span["search_calls"]:  # only spans with at least one search are useful
                all_spans.append(span)

    # Bucket per the acceptance criteria. Each bucket sampled separately
    # so the union covers the diversity requested.
    buckets: dict[str, list[dict]] = defaultdict(list)
    for s in all_spans:
        proj = s["project_slug"]
        n_searches = len(s["search_calls"])
        has_resolve = bool(s["resolve_calls"])
        zero_result_only = (
            all(not c["source_refs"] for c in s["search_calls"])
            and not s["follow_calls"]
        )
        if has_resolve:
            buckets["with_resolve"].append(s)
        if n_searches >= 20:
            buckets["long_span"].append(s)
        if n_searches < 5:
            buckets["short_span"].append(s)
        if zero_result_only:
            buckets["zero_result_no_answer"].append(s)
        buckets[f"proj::{proj}"].append(s)

    # Quota: at least 5 with_resolve, 5 zero_result_no_answer; at least
    # 5 per project for the top three projects by span count; rest filled
    # from a stratified mix.
    chosen: dict[str, dict] = {}
    def take(bucket_name: str, n: int) -> None:
        pool = [s for s in buckets.get(bucket_name, []) if s["promptId"] not in chosen]
        rng.shuffle(pool)
        for s in pool[:n]:
            chosen[s["promptId"]] = s

    take("with_resolve", 7)
    take("zero_result_no_answer", 5)
    take("long_span", 5)
    take("short_span", 5)
    # Project diversity: top 3 by span count.
    proj_sizes = sorted(
        ((k.removeprefix("proj::"), len(v)) for k, v in buckets.items() if k.startswith("proj::")),
        key=lambda x: -x[1],
    )
    for proj, _ in proj_sizes[:4]:
        take(f"proj::{proj}", 4)

    # Fill to target_n with random remaining
    remaining = [s for s in all_spans if s["promptId"] not in chosen]
    rng.shuffle(remaining)
    for s in remaining:
        if len(chosen) >= target_n:
            break
        chosen[s["promptId"]] = s

    out_dir.mkdir(parents=True, exist_ok=True)
    sample_path = out_dir / "spans_sample.json"
    with sample_path.open("w") as fh:
        json.dump({"meta": {"target_n": target_n, "seed": seed,
                            "actual_n": len(chosen),
                            "buckets": {k: len(v) for k, v in buckets.items()}},
                   "spans": list(chosen.values())}, fh, indent=2)
    print(f"sampled {len(chosen)} spans -> {sample_path}")
    print("bucket totals (all available, not just sampled):")
    for k in sorted(buckets, key=lambda x: -len(buckets[x])):
        if not k.startswith("proj::"):
            print(f"  {k:30s} {len(buckets[k]):5d}")
    print("per-project totals:")
    for proj, sz in proj_sizes:
        print(f"  {proj:42s} {sz:5d}")


def summarize(span_json: Path, prompt_id: str) -> None:
    with span_json.open() as fh:
        data = json.load(fh)
    for s in data["spans"]:
        if s["promptId"] == prompt_id:
            print(json.dumps(s, indent=2)[:8000])
            return
    print(f"promptId not found in sample: {prompt_id}", file=sys.stderr)


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("inventory", help="per-project span counts")
    p_sample = sub.add_parser("sample", help="produce a sampled span set")
    p_sample.add_argument("--out-dir", default=str(OUT_DIR_DEFAULT), type=Path)
    p_sample.add_argument("--target-n", type=int, default=40)
    p_sample.add_argument("--seed", type=int, default=17)
    p_show = sub.add_parser("summarize", help="dump one span by promptId")
    p_show.add_argument("--span-json", required=True, type=Path)
    p_show.add_argument("--prompt-id", required=True)
    args = p.parse_args(argv)
    if args.cmd == "inventory":
        inventory()
    elif args.cmd == "sample":
        sample(args.out_dir, args.target_n, args.seed)
    elif args.cmd == "summarize":
        summarize(args.span_json, args.prompt_id)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
