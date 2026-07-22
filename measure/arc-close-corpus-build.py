#!/usr/bin/env python3
"""
Build the labelled corpus of arc-close filing review decisions
for chain arc-close-filing-review-dedupe-and-noise-reduction F1.

Outputs:
  measure/arc-close-corpus.jsonl — one JSON object per actionable
    decision (skip nothing_to_file). Each row includes the proposed
    payload, surrounding context (arc summary, session_id, triggers),
    automated heuristic labels, and a placeholder for the
    real-signal determination (filled by a separate query pass).

Usage:
  python3 measure/arc-close-corpus-build.py
"""

from __future__ import annotations

import json
import re
import sqlite3
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
DB_PATH = REPO_ROOT / "data" / "toolkit.db"
OUT_PATH = REPO_ROOT / "measure" / "arc-close-corpus.jsonl"

# --- Heuristic patterns for automated category labels --------------

OPERATOR_ERROR_MARKERS = [
    "binary not found",
    "file not found",
    "forgot to run",
    "missing in expected location",
    "not found in expected",
    "before the command could be executed",
    "required a workaround",
    "had to be rebuilt",
]

INSUFFICIENT_PAYLOAD_TITLE_REGEXES = [
    re.compile(r"^Add (a )?regression test for [A-Za-z\-_]+$"),
    re.compile(r"^Refactor [A-Za-z\-_]+$"),
    re.compile(r"^Document [A-Za-z\-_]+$"),
]

# Test-docstring-restatement heuristics (forge_vault_note category C):
# body contains test-comment markers, has high code-block ratio, or
# matches a "# Title\n\n## Section\n\n..." synthesis-light shape.
def looks_like_test_docstring_restatement(body: str) -> bool:
    if not body:
        return False
    lines = body.splitlines()
    if not lines:
        return False
    code_block_lines = sum(1 for line in lines if line.strip().startswith("```"))
    # >60% code-block content: count lines inside ``` blocks
    in_block = False
    block_line_count = 0
    for line in lines:
        if line.strip().startswith("```"):
            in_block = not in_block
            continue
        if in_block:
            block_line_count += 1
    if len(lines) > 0 and (block_line_count / len(lines)) > 0.6:
        return True
    # Test-comment markers
    test_markers = ["// @blurb", "expect(", "t.Errorf", "t.Fatalf", "TestHandle"]
    marker_hits = sum(1 for m in test_markers if m in body)
    if marker_hits >= 2:
        return True
    # Three-sentence body that paraphrases a single docstring/commit subject
    # (low novel-synthesis signal — heuristic check on body length)
    if len(body) < 400 and "\n\n" not in body:
        return True
    return False


def classify_decision_heuristic(action: str, payload: dict) -> list[str]:
    """Return zero or more automated category tags. Manual review can override."""
    tags = []
    title = (payload.get("title") or "").strip()
    body = payload.get("body") or ""
    problem = payload.get("problem_statement") or ""

    if action == "forge_bug":
        for marker in OPERATOR_ERROR_MARKERS:
            if marker.lower() in problem.lower():
                tags.append("D-operator-error")
                break
    if action == "forge_suggestion":
        for rgx in INSUFFICIENT_PAYLOAD_TITLE_REGEXES:
            if rgx.match(title) and len(problem) < 200:
                tags.append("F-insufficient-payload-boilerplate")
                break
    if action == "forge_vault_note":
        if looks_like_test_docstring_restatement(body):
            tags.append("C-test-docstring-restatement")
    return tags


# --- Main extraction -----------------------------------------------

def main() -> int:
    con = sqlite3.connect(str(DB_PATH))
    con.row_factory = sqlite3.Row
    cur = con.cursor()

    # Pull from pending_decisions (the canonical store for the
    # proposed payloads — the events-table payload only carries
    # {action, confidence, reasoning} for audit, not the to-forge
    # payload itself). Joined with events to get the arc_summary +
    # triggers context.
    cur.execute("""
        SELECT
            pd.id AS pd_id,
            pd.event_id,
            pd.created_at AS ts,
            pd.target_session_id AS session_id,
            pd.arc_summary,
            pd.triggers_json,
            pd.decisions_json,
            pd.dispatched_at,
            pd.dispatch_error
        FROM pending_decisions pd
        ORDER BY pd.created_at ASC
    """)
    pd_rows = cur.fetchall()
    print(f"pending_decisions: {len(pd_rows)}", file=sys.stderr)

    out_rows = []
    for pd in pd_rows:
        decisions = json.loads(pd["decisions_json"] or "[]")
        for idx, d in enumerate(decisions):
            action = d.get("action")
            if action == "nothing_to_file":
                continue
            decision_payload = d.get("payload") or {}
            heuristic_tags = classify_decision_heuristic(action, decision_payload)
            out_rows.append({
                "pending_decision_id": pd["pd_id"],
                "event_id": pd["event_id"],
                "ts": pd["ts"],
                "session_id": pd["session_id"],
                "arc_summary": pd["arc_summary"],
                "triggers": json.loads(pd["triggers_json"] or "[]"),
                "decision_idx": idx,
                "action": action,
                "confidence": d.get("confidence"),
                "reasoning": d.get("reasoning"),
                "decision_payload": decision_payload,
                "dispatched_at": pd["dispatched_at"],
                "dispatch_error": pd["dispatch_error"],
                "heuristic_tags": heuristic_tags,
                # Filled by the real-signal-determination pass:
                "real_signal": None,
                "manual_category": None,
            })

    print(f"actionable decisions: {len(out_rows)}", file=sys.stderr)
    OUT_PATH.parent.mkdir(parents=True, exist_ok=True)
    with OUT_PATH.open("w") as f:
        for row in out_rows:
            f.write(json.dumps(row) + "\n")
    print(f"wrote {OUT_PATH}", file=sys.stderr)

    # Summary stats
    by_action = {}
    by_heuristic = {}
    for r in out_rows:
        by_action[r["action"]] = by_action.get(r["action"], 0) + 1
        for tag in r["heuristic_tags"]:
            by_heuristic[tag] = by_heuristic.get(tag, 0) + 1
    print("\nby action:", file=sys.stderr)
    for k, v in sorted(by_action.items(), key=lambda x: -x[1]):
        print(f"  {k}: {v}", file=sys.stderr)
    print("\nheuristic tags (automated first-pass):", file=sys.stderr)
    for k, v in sorted(by_heuristic.items(), key=lambda x: -x[1]):
        print(f"  {k}: {v}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
