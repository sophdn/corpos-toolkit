#!/usr/bin/env python3
"""
Replay the arc-close corpus through F4's CheckBoilerplate rules
(reimplemented in Python — same constants as
go/internal/arcreview/validation.go) to measure how many of the
F1-tagged noise decisions F4 actually catches.

Usage:
  python3 measure/arc-close-corpus-f4-replay.py
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
IN_PATH = REPO_ROOT / "measure" / "arc-close-corpus.jsonl"

# ---- Constants mirroring validation.go ----------------------------

VAULT_NOTE_CODE_BLOCK_RATIO_THRESHOLD = 0.60
VAULT_NOTE_TEST_MARKER_MIN_HITS = 2
VAULT_NOTE_SHORT_BODY_MAX_LEN = 400
SUGGESTION_SHORT_PROBLEM_MIN_LEN = 200

VAULT_NOTE_TEST_MARKERS = [
    "// @blurb",
    "expect(",
    "t.Errorf",
    "t.Fatalf",
    "t.Run",
    "TestHandle",
    "func Test",
    "assert!(",
    "#[test]",
]

BUG_OPERATOR_ERROR_MARKERS = [
    "binary not found",
    "file not found",
    "forgot to run",
    "missing in expected location",
    "not found in expected",
    "before the command could be executed",
    "required a workaround",
    "had to be rebuilt",
]

SUGGESTION_GENERIC_TITLE_REGEXES = [
    re.compile(r"^Add (a )?regression test for [A-Za-z\-_]+$"),
    re.compile(r"^Refactor [A-Za-z\-_]+$"),
    re.compile(r"^Document [A-Za-z\-_]+$"),
    re.compile(r"^Improve [A-Za-z\-_ ]{1,30}$"),
]

# F4 tuning rules added 2026-05-21 post-live-observation. Mirror
# the Go regexes in go/internal/arcreview/validation.go.
VAULT_NOTE_DIARY_STARTER_REGEXES = [
    # v1 (commit 59a8f2a)
    re.compile(r"^\s*this note captures (the|some|a)?", re.IGNORECASE),
    re.compile(r"^\s*documenting (the|a|some)?", re.IGNORECASE),
    re.compile(r"^\s*(this|the) (decision|learning|reference) documents", re.IGNORECASE),
    # v2 (F5 retrospective; mirrors validation.go additions)
    re.compile(r"^\s*during the [\w\- ]{0,30}(process|session|implementation|cleanup|attempt|development|migration|sweep|chain|triage|conversation|task|work)\b", re.IGNORECASE),
    re.compile(r"^\s*the (process|approach|method|technique|implementation|strategy) for ", re.IGNORECASE),
    re.compile(r"^\s*this note (documents|describes|records|serves)", re.IGNORECASE),
    re.compile(r"^\s*(this|the) (note|decision|learning|reference) (documents|describes|captures|serves as)", re.IGNORECASE),
    re.compile(r"^\s*documents the .{0,40}(process|approach|strategy|method)", re.IGNORECASE),
    re.compile(r"^\s*the .{0,40} (issue|problem|gotcha) (is|was) a recurring", re.IGNORECASE),
]

VAULT_NOTE_OUTCOME_PARAPHRASE_REGEXES = [
    re.compile(r"was tested.{0,40}showing \d+%", re.IGNORECASE),
    re.compile(r"improvement over .{0,40}baseline", re.IGNORECASE),
    re.compile(r"successfully (implemented|tested|committed|landed|built|ran|verified)", re.IGNORECASE),
    re.compile(r"were (successfully )?(implemented|tested|committed|landed|added)", re.IGNORECASE),
]


def code_block_ratio(body: str) -> float:
    if not body:
        return 0.0
    lines = body.split("\n")
    in_block = False
    inside = 0
    for line in lines:
        trimmed = line.strip()
        if trimmed.startswith("```"):
            in_block = not in_block
            continue
        if in_block:
            inside += 1
    return inside / len(lines) if lines else 0.0


def check_boilerplate(action: str, payload: dict) -> str:
    if action == "forge_vault_note":
        body = payload.get("body") or ""
        title = payload.get("title") or ""
        if not body:
            return ""
        body_opener = body[:80]
        for rgx in VAULT_NOTE_DIARY_STARTER_REGEXES:
            if rgx.match(title) or rgx.match(body_opener):
                return "vault_note_implementation_diary_starter"
        for rgx in VAULT_NOTE_OUTCOME_PARAPHRASE_REGEXES:
            if rgx.search(body):
                return "vault_note_outcome_paraphrase"
        marker_hits = sum(1 for m in VAULT_NOTE_TEST_MARKERS if m in body)
        if marker_hits >= VAULT_NOTE_TEST_MARKER_MIN_HITS:
            return "vault_note_test_restatement"
        if code_block_ratio(body) > VAULT_NOTE_CODE_BLOCK_RATIO_THRESHOLD:
            return "vault_note_high_code_block_ratio"
        if len(body) < VAULT_NOTE_SHORT_BODY_MAX_LEN and "\n\n" not in body:
            return "vault_note_too_short_no_paragraph_break"
        return ""
    if action == "forge_bug":
        problem = (payload.get("problem_statement") or "").lower()
        for marker in BUG_OPERATOR_ERROR_MARKERS:
            if marker in problem:
                return "bug_operator_error_marker"
        return ""
    if action == "forge_suggestion":
        title = (payload.get("title") or "").strip()
        problem = payload.get("problem_statement") or ""
        for rgx in SUGGESTION_GENERIC_TITLE_REGEXES:
            if rgx.match(title) and len(problem) < SUGGESTION_SHORT_PROBLEM_MIN_LEN:
                return "suggestion_generic_title_no_specifics"
        if "YYYY-MM-DD" in (payload.get("source") or ""):
            return "suggestion_placeholder_date_in_source"
        return ""
    return ""


def main() -> int:
    rows = [json.loads(l) for l in IN_PATH.open()]
    by_action = {}
    by_reason = {}
    rejected_total = 0
    f1_tagged_count = 0
    f1_tagged_rejected_by_f4 = 0

    for r in rows:
        action = r["action"]
        payload = r.get("decision_payload") or {}
        reason = check_boilerplate(action, payload)
        f1_tags = r.get("heuristic_tags") or []
        f1_in_f4_scope = any(
            t in ("C-test-docstring-restatement", "D-operator-error",
                  "F-insufficient-payload-boilerplate")
            for t in f1_tags
        )
        if f1_in_f4_scope:
            f1_tagged_count += 1
        if reason:
            rejected_total += 1
            by_reason[reason] = by_reason.get(reason, 0) + 1
            by_action[action] = by_action.get(action, 0) + 1
            if f1_in_f4_scope:
                f1_tagged_rejected_by_f4 += 1

    print(f"total corpus rows: {len(rows)}")
    print(f"F4 rejected: {rejected_total} ({rejected_total*100//len(rows)}%)")
    print()
    print("by rejection reason:")
    for k, v in sorted(by_reason.items(), key=lambda x: -x[1]):
        print(f"  {k}: {v}")
    print()
    print("by action (rejected):")
    for k, v in sorted(by_action.items(), key=lambda x: -x[1]):
        print(f"  {k}: {v}")
    print()
    print("F1-tagged-in-F4-scope: {} rows".format(f1_tagged_count))
    print(f"  caught by F4: {f1_tagged_rejected_by_f4} ({f1_tagged_rejected_by_f4*100//max(f1_tagged_count,1)}%)")
    print()
    # Disagreements (F1 tagged but F4 didn't reject; or vice versa)
    disagreements_f1_only = 0
    disagreements_f4_only = 0
    for r in rows:
        action = r["action"]
        payload = r.get("decision_payload") or {}
        reason = check_boilerplate(action, payload)
        f1_tags = r.get("heuristic_tags") or []
        f1_in_f4_scope = any(
            t in ("C-test-docstring-restatement", "D-operator-error",
                  "F-insufficient-payload-boilerplate")
            for t in f1_tags
        )
        if f1_in_f4_scope and not reason:
            disagreements_f1_only += 1
        elif reason and not f1_in_f4_scope:
            disagreements_f4_only += 1
    print(f"disagreements:")
    print(f"  F1 tagged but F4 passes: {disagreements_f1_only}")
    print(f"  F4 rejects but F1 didn't tag: {disagreements_f4_only}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
