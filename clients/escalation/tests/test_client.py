"""Tests for EscalationClient request shaping + response parsing (no live server).

A fake transport records the (path, body) calls and returns canned responses, so
these tests pin the wire contract the client speaks to ``POST /mcp/admin``.
"""

from __future__ import annotations

from typing import Any

import pytest

from escalation.client import EscalationClient, EscalationError


class FakeTransport:
    """Records calls and returns queued responses in order."""

    def __init__(self, responses: list[Any]) -> None:
        self.responses = responses
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def __call__(self, path: str, body: dict[str, Any]) -> Any:
        self.calls.append((path, body))
        return self.responses.pop(0)


def test_list_thresholds_parses_rows() -> None:
    rows = [
        {
            "trigger_kind": "retry_exhaustion",
            "threshold_value": 2,
            "enabled": True,
            "de_escalation_turns": 2,
        },
        {
            "trigger_kind": "low_confidence",
            "threshold_value": 0.35,
            "enabled": True,
            "de_escalation_turns": 3,
        },
    ]
    transport = FakeTransport([rows])
    client = EscalationClient("http://x", transport=transport)
    config = client.list_thresholds("proj")
    assert config.get("retry_exhaustion") is not None
    assert config.get("low_confidence").threshold_value == 0.35  # type: ignore[union-attr]
    assert config.de_escalation_turns == 3
    path, body = transport.calls[0]
    assert path == "/mcp/admin"
    assert body["action"] == "escalation_threshold_list"
    assert body["params"] == {"project_id": "proj"}


def test_set_threshold_sends_rationale_and_optional_fields() -> None:
    transport = FakeTransport([{"ok": True, "trigger_kind": "retry_exhaustion"}])
    client = EscalationClient("http://x", transport=transport)
    client.set_threshold(
        "retry_exhaustion",
        4,
        project_id="p",
        de_escalation_turns=3,
        rationale="tightening for a flaky workload",
    )
    _, body = transport.calls[0]
    assert body["action"] == "escalation_threshold_set"
    assert body["rationale"] == "tightening for a flaky workload"
    assert body["params"]["threshold_value"] == 4
    assert body["params"]["de_escalation_turns"] == 3
    assert "enabled" not in body["params"]  # omitted -> preserved server-side


def test_propose_returns_event_id_and_omits_absent_optionals() -> None:
    transport = FakeTransport([{"ok": True, "event_id": "evt-123"}])
    client = EscalationClient("http://x", transport=transport)
    event_id = client.propose(
        trigger="retry_exhaustion",
        from_model="deepseek-v4-pro",
        to_model="claude-opus-4-7",
        session_id="s1",
        state_before="cheap",
        state_after="escalated",
        turn_index=4,
        fired_threshold=2.0,
        reason="retry budget exhausted",
    )
    assert event_id == "evt-123"
    _, body = transport.calls[0]
    assert body["action"] == "escalation_propose"
    assert body["rationale"] == "retry budget exhausted"
    assert body["params"]["fired_threshold"] == 2.0
    assert "project_id" not in body["params"]  # absent optional omitted
    assert "trigger_detail" not in body["params"]


def test_project_scope_added_to_envelope() -> None:
    transport = FakeTransport([[]])
    client = EscalationClient("http://x", project="mcp-servers", transport=transport)
    client.list_thresholds()
    _, body = transport.calls[0]
    assert body["project"] == "mcp-servers"


def test_error_envelope_raises() -> None:
    transport = FakeTransport([{"error": "rationale_required", "field": "rationale"}])
    client = EscalationClient("http://x", transport=transport)
    with pytest.raises(EscalationError):
        client.set_threshold("retry_exhaustion", 2, rationale="")


def test_propose_without_event_id_raises() -> None:
    transport = FakeTransport([{"ok": True}])
    client = EscalationClient("http://x", transport=transport)
    with pytest.raises(EscalationError):
        client.propose(
            trigger="low_confidence",
            from_model="q",
            to_model="o",
            session_id="s",
            state_before="cheap",
            state_after="escalated",
        )
