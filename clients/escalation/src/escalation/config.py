"""Effective escalation-threshold config loaded from toolkit-server.

Mirrors the ``escalation_thresholds`` rows the ``admin.escalation_threshold_list``
action returns — one ``TriggerThreshold`` per trigger kind, already merged so a
project-specific override shadows the global default (see
``docs/ORCHESTRATOR_ESCALATION.md`` §6).
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any

#: The five closed trigger kinds — mirrors the DB CHECK constraint in
#: migration 080 and the enum in ``blueprints/events/EscalationProposed.json``.
TRIGGER_KINDS: tuple[str, ...] = (
    "retry_exhaustion",
    "low_confidence",
    "repeated_tool_error",
    "parse_failure",
    "explicit_handoff",
)

#: Default hysteresis K when no config row carries one.
DEFAULT_DE_ESCALATION_TURNS = 2


@dataclass(frozen=True, slots=True)
class TriggerThreshold:
    """One trigger's tuned threshold.

    Attributes:
        trigger_kind: One of :data:`TRIGGER_KINDS`.
        threshold_value: The trigger's threshold. A count for
            retry_exhaustion / repeated_tool_error / parse_failure /
            explicit_handoff; a confidence floor in ``[0, 1]`` for
            low_confidence.
        enabled: A disabled trigger never fires regardless of signals.
        de_escalation_turns: Hysteresis K carried on the row (router-level;
            kept uniform across a project's rows by the set action).
    """

    trigger_kind: str
    threshold_value: float
    enabled: bool = True
    de_escalation_turns: int = DEFAULT_DE_ESCALATION_TURNS

    @classmethod
    def from_row(cls, row: dict[str, Any]) -> TriggerThreshold:
        """Build from one ``escalation_threshold_list`` JSON row.

        Args:
            row: A decoded JSON object with at least ``trigger_kind`` and
                ``threshold_value`` keys.

        Returns:
            The parsed threshold, defaulting ``enabled``/``de_escalation_turns``
            when the row omits them.
        """
        return cls(
            trigger_kind=str(row["trigger_kind"]),
            threshold_value=float(row["threshold_value"]),
            enabled=bool(row.get("enabled", True)),
            de_escalation_turns=int(row.get("de_escalation_turns", DEFAULT_DE_ESCALATION_TURNS)),
        )


@dataclass(frozen=True, slots=True)
class ThresholdConfig:
    """The effective per-trigger config for a project.

    Attributes:
        thresholds: ``trigger_kind`` -> :class:`TriggerThreshold`.
    """

    thresholds: dict[str, TriggerThreshold]

    @classmethod
    def from_rows(cls, rows: list[dict[str, Any]]) -> ThresholdConfig:
        """Build from the ``escalation_threshold_list`` response array."""
        return cls({str(r["trigger_kind"]): TriggerThreshold.from_row(r) for r in rows})

    @classmethod
    def defaults(cls) -> ThresholdConfig:
        """The built-in defaults (docs §2), for a harness running without a server."""
        return cls(
            {
                "retry_exhaustion": TriggerThreshold("retry_exhaustion", 2.0),
                "low_confidence": TriggerThreshold("low_confidence", 0.35),
                "repeated_tool_error": TriggerThreshold("repeated_tool_error", 3.0),
                "parse_failure": TriggerThreshold("parse_failure", 2.0),
                "explicit_handoff": TriggerThreshold("explicit_handoff", 1.0),
            }
        )

    def get(self, trigger_kind: str) -> TriggerThreshold | None:
        """Return the threshold for a kind, or ``None`` when unconfigured."""
        return self.thresholds.get(trigger_kind)

    @property
    def de_escalation_turns(self) -> int:
        """The router-level hysteresis K.

        Taken as the max ``de_escalation_turns`` across enabled rows (they are
        uniform by construction of the set action); falls back to
        :data:`DEFAULT_DE_ESCALATION_TURNS` when no enabled row carries one.
        """
        values = [t.de_escalation_turns for t in self.thresholds.values() if t.enabled]
        return max(values) if values else DEFAULT_DE_ESCALATION_TURNS
