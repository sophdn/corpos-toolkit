"""Tests for the EscalationRouter state machine + de-escalation hysteresis."""

from __future__ import annotations

from escalation.config import ThresholdConfig
from escalation.router import EscalationDecision, EscalationRouter, make_emitter
from escalation.triggers import TurnSignals


def _router(**kwargs: object) -> EscalationRouter:
    return EscalationRouter(
        cheap_model="deepseek-v4-pro",
        strong_model="claude-opus-4-7",
        config=ThresholdConfig.defaults(),  # K = 2
        session_id="sess-1",
        **kwargs,  # type: ignore[arg-type]
    )


def test_starts_cheap() -> None:
    r = _router()
    assert r.state == "cheap"
    assert r.next_model() == "deepseek-v4-pro"


def test_escalates_on_retry_exhaustion_and_switches_model() -> None:
    r = _router()
    decision = r.observe(TurnSignals(retries_used=2, detail="unit=at-3"))
    assert decision is not None
    assert decision.edge == "escalate"
    assert decision.trigger == "retry_exhaustion"
    assert decision.from_model == "deepseek-v4-pro"
    assert decision.to_model == "claude-opus-4-7"
    assert decision.state_before == "cheap"
    assert decision.state_after == "escalated"
    assert decision.fired_threshold == 2.0
    # next turn runs on the strong model
    assert r.state == "escalated"
    assert r.next_model() == "claude-opus-4-7"


def test_no_escalation_below_threshold() -> None:
    r = _router()
    assert r.observe(TurnSignals(retries_used=1)) is None
    assert r.state == "cheap"
    assert r.next_model() == "deepseek-v4-pro"


def test_de_escalates_after_k_clean_turns() -> None:
    r = _router()  # K = 2
    r.observe(TurnSignals(retries_used=2))  # escalate
    assert r.next_model() == "claude-opus-4-7"
    # one clean turn: still escalated (streak 1 < K)
    assert r.observe(TurnSignals()) is None
    assert r.state == "escalated"
    assert r.next_model() == "claude-opus-4-7"
    # second clean turn: K reached -> de-escalate
    decision = r.observe(TurnSignals())
    assert decision is not None
    assert decision.edge == "de_escalate"
    assert decision.trigger is None
    assert decision.from_model == "claude-opus-4-7"
    assert decision.to_model == "deepseek-v4-pro"
    assert decision.state_after == "de_escalated"
    assert r.state == "cheap"
    assert r.next_model() == "deepseek-v4-pro"


def test_trigger_while_escalated_resets_streak() -> None:
    r = _router()  # K = 2
    r.observe(TurnSignals(retries_used=2))  # escalate
    r.observe(TurnSignals())  # clean, streak -> 1
    assert r.clean_streak == 1
    # a trigger while escalated resets the streak and does not re-emit
    assert r.observe(TurnSignals(tool_errors=3)) is None
    assert r.state == "escalated"
    assert r.clean_streak == 0
    # now it takes a fresh K clean turns to de-escalate
    assert r.observe(TurnSignals()) is None
    decision = r.observe(TurnSignals())
    assert decision is not None and decision.edge == "de_escalate"


def test_on_escalation_fires_only_on_escalate_edge() -> None:
    seen: list[EscalationDecision] = []
    r = _router(on_escalation=seen.append)
    r.observe(TurnSignals(retries_used=2))  # escalate -> hook fires
    r.observe(TurnSignals())  # clean
    r.observe(TurnSignals())  # de-escalate -> hook must NOT fire
    assert len(seen) == 1
    assert seen[0].edge == "escalate"


def test_turn_index_advances_and_is_stamped() -> None:
    r = _router()
    r.observe(TurnSignals())  # turn 0, no fire
    r.observe(TurnSignals())  # turn 1, no fire
    decision = r.observe(TurnSignals(explicit_handoff=1))  # turn 2, escalate
    assert decision is not None
    assert decision.turn_index == 2
    assert r.turn_index == 3


class _RecordingClient:
    """Captures propose() kwargs without a server (duck-types EscalationClient)."""

    def __init__(self) -> None:
        self.calls: list[dict[str, object]] = []

    def propose(self, **kwargs: object) -> str:
        self.calls.append(kwargs)
        return f"evt-{len(self.calls)}"


def test_make_emitter_proposes_escalate_edge() -> None:
    client = _RecordingClient()
    r = _router(on_escalation=make_emitter(client, project_id="mcp-servers"))  # type: ignore[arg-type]
    r.observe(TurnSignals(retries_used=2, detail="unit=at-3"))
    assert len(client.calls) == 1
    call = client.calls[0]
    assert call["trigger"] == "retry_exhaustion"
    assert call["from_model"] == "deepseek-v4-pro"
    assert call["to_model"] == "claude-opus-4-7"
    assert call["state_after"] == "escalated"
    assert call["project_id"] == "mcp-servers"
    assert call["reason"]  # synthesised, non-empty


def test_full_arc_escalate_then_de_escalate() -> None:
    client = _RecordingClient()
    r = _router(on_escalation=make_emitter(client))  # type: ignore[arg-type]
    # cheap -> escalate
    assert r.next_model() == "deepseek-v4-pro"
    r.observe(TurnSignals(confidence=0.1))
    assert r.next_model() == "claude-opus-4-7"
    # K=2 clean turns -> back to cheap
    r.observe(TurnSignals())
    r.observe(TurnSignals())
    assert r.next_model() == "deepseek-v4-pro"
    # only the escalate edge was emitted
    assert len(client.calls) == 1
