#!/usr/bin/env python3
"""
Classify the arc-close corpus (built by arc-close-corpus-build.py)
with dedupe-detection heuristics. Operates on the JSONL output
from the build pass; updates each row with:

  - dup_already_filed: matching artifact found in bug_list /
    suggestion_list / vault corpus at or before the decision's ts
    (suggesting the dedupe filter F2 would have caught it).
  - dup_same_session: another decision in the same session_id
    proposed a near-equivalent payload (suggesting F3 would have
    caught it).

Usage:
  python3 measure/arc-close-corpus-classify.py
"""

from __future__ import annotations

import json
import re
import sqlite3
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DB_PATH = REPO_ROOT / "data" / "toolkit.db"
IN_OUT_PATH = REPO_ROOT / "measure" / "arc-close-corpus.jsonl"


def tokens(s: str) -> set[str]:
    """Lowercase alphanumeric+hyphen tokens, length >= 3."""
    if not s:
        return set()
    raw = re.findall(r"[a-z0-9][a-z0-9\-_]{2,}", s.lower())
    # Drop stopwords / very-common words
    stop = {
        "and", "or", "the", "for", "into", "with", "this", "that",
        "from", "session", "retro", "after", "before", "task", "bug",
        "via", "should", "may", "could", "would", "via", "their", "are",
        "not", "but", "all", "now", "also",
    }
    return {t for t in raw if t not in stop}


def jaccard(a: set[str], b: set[str]) -> float:
    if not a and not b:
        return 0.0
    inter = a & b
    union = a | b
    if not union:
        return 0.0
    return len(inter) / len(union)


def load_existing_corpora(con: sqlite3.Connection) -> dict:
    """Load all existing bugs / suggestions / vault entries with their
    titles + slugs + filed_at, so we can spot dup-already-filed cases.
    Returns dict mapping kind -> list of (slug, title, filed_at)."""
    out = {"bug": [], "suggestion": [], "vault": []}
    cur = con.cursor()
    cur.execute("SELECT slug, title, filed_at FROM proj_current_bugs")
    out["bug"] = cur.fetchall()
    cur.execute("SELECT slug, title, filed_at FROM proj_current_suggestions")
    out["suggestion"] = cur.fetchall()
    # Vault entries: use knowledge_pointers as the index proxy.
    cur.execute("""
        SELECT source_ref AS slug,
               COALESCE(question, '') AS title,
               '' AS filed_at
        FROM knowledge_pointers
        WHERE source_type = 'vault-note'
    """)
    out["vault"] = cur.fetchall()
    return out


ACTION_TO_KIND = {
    "forge_bug": "bug",
    "forge_suggestion": "suggestion",
    "forge_vault_note": "vault",
}


def check_already_filed(decision: dict, corpora: dict) -> tuple[str, float] | None:
    """Returns (matched_slug, similarity) or None.
    Compares the decision's title against existing artifacts of the
    same kind. Uses Jaccard 0.4 threshold."""
    action = decision["action"]
    kind = ACTION_TO_KIND.get(action)
    if kind is None:
        return None
    payload = decision.get("decision_payload") or {}
    title = payload.get("title") or ""
    if not title:
        return None
    d_tokens = tokens(title)
    if not d_tokens:
        return None
    best = (None, 0.0)
    for existing_slug, existing_title, _ in corpora[kind]:
        e_tokens = tokens(existing_title or "")
        if not e_tokens:
            continue
        sim = jaccard(d_tokens, e_tokens)
        if sim > best[1]:
            best = (existing_slug, sim)
    if best[1] >= 0.4:
        return best
    return None


def check_same_session_dup(decision: dict, all_rows: list, idx: int) -> tuple[int, float] | None:
    """Returns (other_row_index, similarity) or None.
    Compares against earlier-in-time decisions from the same session_id."""
    sid = decision["session_id"]
    if not sid:
        return None
    d_payload = decision.get("decision_payload") or {}
    d_title = d_payload.get("title") or d_payload.get("name") or ""
    d_problem = d_payload.get("problem_statement") or d_payload.get("body") or ""
    d_tokens = tokens(d_title) | tokens(d_problem)
    if not d_tokens:
        return None
    best = (None, 0.0)
    for j, other in enumerate(all_rows):
        if j >= idx:
            break  # only earlier-in-time
        if other["session_id"] != sid:
            continue
        if other["action"] != decision["action"]:
            continue
        o_payload = other.get("decision_payload") or {}
        o_title = o_payload.get("title") or o_payload.get("name") or ""
        o_problem = o_payload.get("problem_statement") or o_payload.get("body") or ""
        o_tokens = tokens(o_title) | tokens(o_problem)
        if not o_tokens:
            continue
        sim = jaccard(d_tokens, o_tokens)
        if sim > best[1]:
            best = (j, sim)
    if best[1] >= 0.4:
        return best
    return None


def main() -> int:
    con = sqlite3.connect(str(DB_PATH))
    con.row_factory = sqlite3.Row
    corpora = load_existing_corpora(con)
    print(f"corpora loaded: bug={len(corpora['bug'])} suggestion={len(corpora['suggestion'])} vault={len(corpora['vault'])}", file=sys.stderr)

    rows = [json.loads(line) for line in IN_OUT_PATH.open()]
    print(f"corpus rows: {len(rows)}", file=sys.stderr)

    # Sort chronologically so same-session-dup check works correctly.
    rows.sort(key=lambda r: r["ts"])

    for idx, row in enumerate(rows):
        match = check_already_filed(row, corpora)
        if match:
            row["dup_already_filed"] = {"slug": match[0], "similarity": round(match[1], 3)}
            row["heuristic_tags"].append("A-already-filed")
        else:
            row["dup_already_filed"] = None

        same_session = check_same_session_dup(row, rows, idx)
        if same_session:
            row["dup_same_session"] = {
                "other_pending_decision_id": rows[same_session[0]]["pending_decision_id"],
                "similarity": round(same_session[1], 3),
            }
            row["heuristic_tags"].append("B-same-session-duplicate")
        else:
            row["dup_same_session"] = None

    # Write back
    with IN_OUT_PATH.open("w") as f:
        for row in rows:
            f.write(json.dumps(row) + "\n")

    # Summary
    print("\nUpdated tag distribution:", file=sys.stderr)
    by_tag = {}
    for r in rows:
        for tag in r["heuristic_tags"]:
            by_tag[tag] = by_tag.get(tag, 0) + 1
    for k, v in sorted(by_tag.items(), key=lambda x: -x[1]):
        print(f"  {k}: {v}", file=sys.stderr)

    # Tagged at all
    tagged = sum(1 for r in rows if r["heuristic_tags"])
    print(f"\ntotal decisions: {len(rows)}", file=sys.stderr)
    print(f"  with >=1 heuristic tag: {tagged} ({tagged*100//len(rows)}%)", file=sys.stderr)
    print(f"  untagged: {len(rows) - tagged} ({(len(rows)-tagged)*100//len(rows)}%)", file=sys.stderr)

    # Per-action tagged-rate
    print("\nPer-action tag rate:", file=sys.stderr)
    by_action_total = {}
    by_action_tagged = {}
    for r in rows:
        a = r["action"]
        by_action_total[a] = by_action_total.get(a, 0) + 1
        if r["heuristic_tags"]:
            by_action_tagged[a] = by_action_tagged.get(a, 0) + 1
    for a in sorted(by_action_total):
        total = by_action_total[a]
        tagged_a = by_action_tagged.get(a, 0)
        print(f"  {a}: {tagged_a}/{total} ({tagged_a*100//total}%)", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
