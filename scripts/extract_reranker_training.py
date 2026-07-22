#!/usr/bin/env python3
"""
extract_reranker_training.py — worked example for chain query-telemetry-substrate TT4.

Reads proj_training_data_for_reranker (TT3's substrate-to-ML bridge) and produces
(query, positive_pointer_id, negative_pointer_id) training triples for cross-encoder
fine-tuning per ~/Documents/files/Already Processed Idea Files/local-ml-roadmap.md
§1.1.

The pipeline writer reads ONE table — no joins. That's the contract docs/PROJECTIONS.md
§6.1 promises and this script demonstrates.

Usage:
    python3 scripts/extract_reranker_training.py --db data/toolkit.db
    python3 scripts/extract_reranker_training.py --db data/toolkit.db --label-set strict
    python3 scripts/extract_reranker_training.py --db data/toolkit.db --label-set recall
    python3 scripts/extract_reranker_training.py --db data/toolkit.db --out triples.jsonl

label-set:
    strict  — positive + hard_negative only (high-precision fine-tune)
    recall  — positive + weakly_positive + hard_negative + negative (recall-leaning)
"""

import argparse
import json
import sqlite3
import sys
from collections import Counter, defaultdict
from pathlib import Path


def load_projection(db_path: Path, query_source: str | None) -> list[dict]:
    """Read proj_training_data_for_reranker as a list of dicts.

    query_source filter is the proactive-injection forward-compat hook from TT1 §8 —
    a future training pipeline differentiating agent-initiated vs proactive
    queries can filter here without re-deriving the discriminator.
    """
    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    sql = """
        SELECT grounding_event_id, query_text, candidate_pointer_id, source_ref,
               candidate_position, label_kind, weight, label_sources,
               query_source, was_injected, prompt_id, span_id
        FROM proj_training_data_for_reranker
    """
    params: tuple = ()
    if query_source:
        sql += " WHERE query_source = ?"
        params = (query_source,)
    sql += " ORDER BY grounding_event_id, candidate_position"
    rows = [dict(r) for r in conn.execute(sql, params).fetchall()]
    conn.close()
    return rows


def label_distribution(rows: list[dict]) -> Counter:
    return Counter(r["label_kind"] for r in rows)


def label_sources_distribution(rows: list[dict]) -> Counter:
    """Per-row label_sources is a JSON array of click_kind strings. Flatten and count."""
    counter: Counter = Counter()
    for r in rows:
        try:
            kinds = json.loads(r["label_sources"]) if r["label_sources"] else []
        except json.JSONDecodeError:
            continue
        for k in kinds:
            counter[k] += 1
    return counter


def pair_triples(rows: list[dict], label_set: str) -> list[tuple[str, int, int]]:
    """Group rows by grounding_event_id, then pair positives with negatives within
    the same query so the cross-encoder learns 'this query → A > B' rather than
    'query → A is good' alone. label_set selects which label kinds count as
    positive vs negative."""
    if label_set == "strict":
        positive_kinds = {"positive"}
        negative_kinds = {"hard_negative"}
    elif label_set == "recall":
        positive_kinds = {"positive", "weakly_positive"}
        negative_kinds = {"hard_negative", "negative"}
    else:
        raise ValueError(f"unknown label_set: {label_set!r}")

    by_query: dict[int, dict[str, list[dict]]] = defaultdict(lambda: {"pos": [], "neg": []})
    for r in rows:
        if r["candidate_pointer_id"] is None:
            continue  # candidate left the pointer index since the search — skip
        if r["label_kind"] in positive_kinds:
            by_query[r["grounding_event_id"]]["pos"].append(r)
        elif r["label_kind"] in negative_kinds:
            by_query[r["grounding_event_id"]]["neg"].append(r)

    triples: list[tuple[str, int, int]] = []
    for ge_id, sides in by_query.items():
        if not sides["pos"] or not sides["neg"]:
            continue
        # One triple per positive × negative pair within the same query. For
        # large fan-out we'd cap, but at homelab scale this is fine.
        for pos in sides["pos"]:
            for neg in sides["neg"]:
                query = pos["query_text"] or ""
                triples.append((query, pos["candidate_pointer_id"], neg["candidate_pointer_id"]))
    return triples


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--db", required=True, type=Path, help="path to toolkit.db")
    parser.add_argument(
        "--label-set",
        choices=("strict", "recall"),
        default="strict",
        help="strict = positive vs hard_negative; recall = include weakly_positive + negative",
    )
    parser.add_argument(
        "--query-source",
        choices=("agent_initiated", "proactive_hook", "dashboard_user", "other"),
        default=None,
        help="filter to one query_source bucket (omit to include all)",
    )
    parser.add_argument(
        "--out",
        type=Path,
        default=None,
        help="write triples as JSONL to this path (omit to print summary only)",
    )
    args = parser.parse_args()

    if not args.db.exists():
        print(f"error: {args.db} does not exist", file=sys.stderr)
        return 2

    rows = load_projection(args.db, args.query_source)
    print(f"proj_training_data_for_reranker rows: {len(rows)}")

    if not rows:
        print("\nNo rows in proj_training_data_for_reranker.")
        print("Expected on a fresh install or before click_kind detection has fired.")
        print("Run the grounding-events-processor on session JSONLs to populate.")
        return 0

    print("\nlabel_kind distribution:")
    for kind, count in sorted(label_distribution(rows).items()):
        print(f"  {kind:18} {count}")

    print("\nlabel_sources composition (click_kinds that fired across rows):")
    src_dist = label_sources_distribution(rows)
    if src_dist:
        for kind, count in sorted(src_dist.items()):
            print(f"  {kind:18} {count}")
    else:
        print("  (no label_sources populated yet — no click_kind detection has fired)")

    triples = pair_triples(rows, args.label_set)
    print(f"\n{args.label_set} triples (query, positive_pointer_id, negative_pointer_id): {len(triples)}")

    if args.out and triples:
        with args.out.open("w") as f:
            for query, pos_id, neg_id in triples:
                f.write(json.dumps({"query": query, "positive": pos_id, "negative": neg_id}) + "\n")
        print(f"wrote {len(triples)} triples to {args.out}")

    if len(triples) < 100:
        print("\n(< 100 triples — the substrate is forward-only-capture; trajectories accumulate")
        print(" from subsequent sessions. Re-run after the grounding-events-processor has folded")
        print(" several days of agent activity for production-scale training data.)")

    return 0


if __name__ == "__main__":
    sys.exit(main())
