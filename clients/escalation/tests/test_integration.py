"""End-to-end integration test against a live toolkit-server.

This is the chain's completion-condition (e) proof, exercised over the real
HTTP MCP wire: a simulated retry-exhaustion fires EscalationProposed, the router
switches the next turn's model, de-escalation is observed after K clean turns,
threshold config round-trips through the admin actions, and the EscalationProposed
event is confirmed landed in the write-side ledger.

It is skipped unless ``TOOLKIT_ESCALATION_BASE_URL`` points at a running daemon.
Bring one up isolated from :3000 with the worktree helper, then run::

    PORT=3994 ./scripts/worktree-mcp.sh --fresh &      # in the repo root
    cd clients/escalation
    TOOLKIT_ESCALATION_BASE_URL=http://localhost:3994 \
      TOOLKIT_ESCALATION_DB=/tmp/toolkit-worktree-orchestrator-tier-escalation-contract.db \
      PYTHONPATH=src pytest tests/test_integration.py -v

The optional ``TOOLKIT_ESCALATION_DB`` enables the direct ledger read-back; when
unset, the test relies on the returned event_id as the landing proof.
"""

from __future__ import annotations

import os
import sqlite3
import uuid

import pytest

from escalation import EscalationClient, EscalationRouter, TurnSignals
from escalation.router import EscalationDecision

BASE_URL = os.environ.get("TOOLKIT_ESCALATION_BASE_URL")
DB_PATH = os.environ.get("TOOLKIT_ESCALATION_DB")
PROJECT = "mcp-servers"

pytestmark = pytest.mark.skipif(
    not BASE_URL,
    reason="set TOOLKIT_ESCALATION_BASE_URL to run the live integration test",
)


def test_threshold_config_round_trips() -> None:
    assert BASE_URL is not None
    client = EscalationClient(BASE_URL, project=PROJECT)
    client.set_threshold(
        "retry_exhaustion",
        2,
        project_id=PROJECT,
        de_escalation_turns=2,
        rationale="integration test: pin retry_exhaustion for the escalation arc",
    )
    config = client.list_thresholds(PROJECT)
    threshold = config.get("retry_exhaustion")
    assert threshold is not None
    assert threshold.threshold_value == 2.0
    assert config.de_escalation_turns == 2


def test_full_escalation_arc_lands_event() -> None:
    assert BASE_URL is not None
    client = EscalationClient(BASE_URL, project=PROJECT)
    config = client.list_thresholds(PROJECT)
    session_id = f"orch-it-{uuid.uuid4().hex[:8]}"

    event_ids: list[str] = []

    def on_escalation(decision: EscalationDecision) -> None:
        assert decision.trigger is not None
        event_ids.append(
            client.propose(
                trigger=decision.trigger,
                from_model=decision.from_model,
                to_model=decision.to_model,
                session_id=decision.session_id,
                state_before=decision.state_before,
                state_after=decision.state_after,
                turn_index=decision.turn_index,
                trigger_detail=decision.trigger_detail,
                fired_threshold=decision.fired_threshold,
                project_id=PROJECT,
                reason=f"integration: {decision.trigger_detail}",
            )
        )

    router = EscalationRouter(
        cheap_model="deepseek-v4-pro",
        strong_model="claude-opus-4-7",
        config=config,
        session_id=session_id,
        on_escalation=on_escalation,
    )

    # Starts on the cheap tier.
    assert router.next_model() == "deepseek-v4-pro"

    # Simulated retry-exhaustion fires the escalation.
    decision = router.observe(TurnSignals(retries_used=2, detail="unit=at-it"))
    assert decision is not None
    assert decision.edge == "escalate"
    # The next turn switches to the strong model.
    assert router.next_model() == "claude-opus-4-7"
    assert len(event_ids) == 1

    # K clean turns -> de-escalation observed.
    k = config.de_escalation_turns
    for _ in range(k):
        router.observe(TurnSignals())
    assert router.next_model() == "deepseek-v4-pro"

    # The EscalationProposed event landed in the write-side ledger.
    if DB_PATH:
        conn = sqlite3.connect(f"file:{DB_PATH}?mode=ro", uri=True)
        try:
            row = conn.execute(
                "SELECT type, entity_kind, entity_slug FROM events WHERE event_id = ?",
                (event_ids[0],),
            ).fetchone()
        finally:
            conn.close()
        assert row is not None, "EscalationProposed event_id not found in the ledger"
        assert row[0] == "EscalationProposed"
        assert row[1] == "orchestrator_session"
        assert row[2] == session_id
