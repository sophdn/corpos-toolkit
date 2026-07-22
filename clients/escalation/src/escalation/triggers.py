"""Per-trigger escalation detectors.

Pure functions: given the signals a harness observed during a turn plus the
effective :class:`~escalation.config.ThresholdConfig`, decide whether any of the
five triggers fired. No I/O, no model calls, no cross-turn state — the router
(:mod:`escalation.router`) owns the windowing and state. See
``docs/ORCHESTRATOR_ESCALATION.md`` §2.
"""

from __future__ import annotations

from dataclasses import dataclass

from .config import ThresholdConfig

#: Evaluation priority when several triggers fire on the same turn — the first
#: match in this order is the one reported. Explicit self-escalation wins, then
#: hard failures (won't-converge), then the soft confidence floor.
PRIORITY: tuple[str, ...] = (
    "explicit_handoff",
    "retry_exhaustion",
    "parse_failure",
    "repeated_tool_error",
    "low_confidence",
)


@dataclass(frozen=True, slots=True)
class TurnSignals:
    """What a harness observed during one turn, in the current escalation window.

    All counts are window-accumulated (the harness/router resets them when the
    window resets); the detector compares each against its threshold without any
    state of its own.

    Attributes:
        retries_used: Failed retries consumed on the current gate-verified unit.
        confidence: The cheap model's self-reported confidence in its chosen
            action, in ``[0, 1]``; ``None`` when unmeasured (low_confidence then
            never fires).
        tool_errors: Structured tool-call errors seen in the window.
        parse_failures: Times the model's output failed to parse into the
            required structured shape.
        explicit_handoff: Count of explicit "hand me to the strong tier" signals
            the model emitted (0 when none).
        detail: Optional free-form evidence appended to the fired trigger's
            detail string.
    """

    retries_used: int = 0
    confidence: float | None = None
    tool_errors: int = 0
    parse_failures: int = 0
    explicit_handoff: int = 0
    detail: str = ""


@dataclass(frozen=True, slots=True)
class TriggerFired:
    """The outcome of a detector firing.

    Attributes:
        trigger_kind: Which trigger fired.
        threshold_value: The threshold it crossed (for the event snapshot).
        detail: Human-readable evidence (not parsed downstream).
    """

    trigger_kind: str
    threshold_value: float
    detail: str


def _fires(kind: str, signals: TurnSignals, threshold: float) -> bool:
    """Whether one trigger's condition holds for the given signals."""
    if kind == "retry_exhaustion":
        return signals.retries_used >= threshold
    if kind == "repeated_tool_error":
        return signals.tool_errors >= threshold
    if kind == "parse_failure":
        return signals.parse_failures >= threshold
    if kind == "explicit_handoff":
        return signals.explicit_handoff >= threshold
    if kind == "low_confidence":
        return signals.confidence is not None and signals.confidence < threshold
    return False


def _detail(kind: str, signals: TurnSignals) -> str:
    """Synthesise per-trigger evidence, appending any caller-supplied detail."""
    base = {
        "retry_exhaustion": f"retries_used={signals.retries_used}",
        "repeated_tool_error": f"tool_errors={signals.tool_errors}",
        "parse_failure": f"parse_failures={signals.parse_failures}",
        "explicit_handoff": f"explicit_handoff={signals.explicit_handoff}",
        "low_confidence": f"confidence={signals.confidence}",
    }.get(kind, kind)
    if signals.detail:
        return f"{base} {signals.detail}"
    return base


def detect(signals: TurnSignals, config: ThresholdConfig) -> TriggerFired | None:
    """Return the highest-priority trigger that fired, or ``None``.

    Only enabled, configured triggers are evaluated. Ties are broken by
    :data:`PRIORITY`.

    Args:
        signals: The turn's observed signals.
        config: The effective per-trigger threshold config.

    Returns:
        The fired trigger, or ``None`` when no trigger's condition held.
    """
    for kind in PRIORITY:
        threshold = config.get(kind)
        if threshold is None or not threshold.enabled:
            continue
        if _fires(kind, signals, threshold.threshold_value):
            return TriggerFired(
                trigger_kind=kind,
                threshold_value=threshold.threshold_value,
                detail=_detail(kind, signals),
            )
    return None
