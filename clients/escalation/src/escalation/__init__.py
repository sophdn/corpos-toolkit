"""Harness-agnostic reference library for the orchestrator-tier escalation contract.

A cheap orchestrator-tier model (DeepSeek V4 Pro, Qwen) drives the conversational
loop; when a trigger fires, the contract proposes handing the next turn to a
strong tier (Opus 4.7), records the proposal as an ``EscalationProposed`` event
through toolkit-server, and de-escalates back after K clean turns. This package
is the reference implementation any harness can import.

See ``docs/ORCHESTRATOR_ESCALATION.md`` in the mcp-servers repo for the contract.
"""

from __future__ import annotations

from .client import EscalationClient, EscalationError
from .config import (
    DEFAULT_DE_ESCALATION_TURNS,
    TRIGGER_KINDS,
    ThresholdConfig,
    TriggerThreshold,
)
from .router import EscalationDecision, EscalationRouter, OnEscalation, make_emitter
from .triggers import PRIORITY, TriggerFired, TurnSignals, detect

__all__ = [
    "DEFAULT_DE_ESCALATION_TURNS",
    "PRIORITY",
    "TRIGGER_KINDS",
    "EscalationClient",
    "EscalationDecision",
    "EscalationError",
    "EscalationRouter",
    "OnEscalation",
    "ThresholdConfig",
    "TriggerFired",
    "TriggerThreshold",
    "TurnSignals",
    "detect",
    "make_emitter",
]
