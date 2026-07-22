"""Tests for the per-trigger detectors."""

from __future__ import annotations

import pytest

from escalation.config import ThresholdConfig, TriggerThreshold
from escalation.triggers import TurnSignals, detect


def _config(**overrides: TriggerThreshold) -> ThresholdConfig:
    base = ThresholdConfig.defaults().thresholds.copy()
    base.update(overrides)
    return ThresholdConfig(base)


@pytest.mark.parametrize(
    ("signals", "expected_kind"),
    [
        (TurnSignals(retries_used=2), "retry_exhaustion"),
        (TurnSignals(retries_used=1), None),
        (TurnSignals(confidence=0.2), "low_confidence"),
        (TurnSignals(confidence=0.35), None),  # floor is exclusive (< 0.35)
        (TurnSignals(confidence=0.9), None),
        (TurnSignals(tool_errors=3), "repeated_tool_error"),
        (TurnSignals(tool_errors=2), None),
        (TurnSignals(parse_failures=2), "parse_failure"),
        (TurnSignals(explicit_handoff=1), "explicit_handoff"),
        (TurnSignals(), None),
    ],
)
def test_detect_single_trigger(signals: TurnSignals, expected_kind: str | None) -> None:
    fired = detect(signals, ThresholdConfig.defaults())
    if expected_kind is None:
        assert fired is None
    else:
        assert fired is not None
        assert fired.trigger_kind == expected_kind


def test_low_confidence_none_never_fires() -> None:
    assert detect(TurnSignals(confidence=None), ThresholdConfig.defaults()) is None


def test_priority_explicit_handoff_wins() -> None:
    # Several conditions hold at once; explicit_handoff is highest priority.
    signals = TurnSignals(retries_used=5, parse_failures=5, explicit_handoff=1, confidence=0.0)
    fired = detect(signals, ThresholdConfig.defaults())
    assert fired is not None
    assert fired.trigger_kind == "explicit_handoff"


def test_priority_retry_over_confidence() -> None:
    signals = TurnSignals(retries_used=5, confidence=0.0)
    fired = detect(signals, ThresholdConfig.defaults())
    assert fired is not None
    assert fired.trigger_kind == "retry_exhaustion"


def test_disabled_trigger_does_not_fire() -> None:
    config = _config(
        retry_exhaustion=TriggerThreshold("retry_exhaustion", 2.0, enabled=False),
    )
    # retry_exhaustion is disabled; nothing else holds → no fire.
    assert detect(TurnSignals(retries_used=9), config) is None


def test_unconfigured_trigger_is_skipped() -> None:
    # A config missing retry_exhaustion entirely must not crash; it just skips.
    config = ThresholdConfig({"low_confidence": TriggerThreshold("low_confidence", 0.3)})
    assert detect(TurnSignals(retries_used=9), config) is None
    assert detect(TurnSignals(confidence=0.1), config) is not None


def test_fired_carries_threshold_and_detail() -> None:
    fired = detect(TurnSignals(retries_used=4, detail="unit=at-3"), ThresholdConfig.defaults())
    assert fired is not None
    assert fired.threshold_value == 2.0
    assert "retries_used=4" in fired.detail
    assert "unit=at-3" in fired.detail


def test_custom_threshold_value() -> None:
    config = _config(retry_exhaustion=TriggerThreshold("retry_exhaustion", 5.0))
    assert detect(TurnSignals(retries_used=4), config) is None
    assert detect(TurnSignals(retries_used=5), config) is not None
