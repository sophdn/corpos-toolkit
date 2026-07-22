"""The cheap <-> strong orchestrator-tier router state machine.

Implements the two-call harness contract from ``docs/ORCHESTRATOR_ESCALATION.md``
§4: the harness calls :meth:`EscalationRouter.observe` after each turn (feeding
the detectors, which may transition state and emit an event) and
:meth:`EscalationRouter.next_model` at the top of each turn (reading the model
that should drive it).

The machine has two resting states — ``cheap`` and ``escalated``. A fired trigger
in the ``cheap`` state escalates (recording an ``EscalationProposed`` event via
the optional emit hook) and the next turn runs on the strong model. Escalation
holds until **K consecutive clean turns** (the ``de_escalation_turns`` hysteresis)
have elapsed, then control returns to the cheap model. Any trigger while escalated
resets the clean streak. See §5 for why the hysteresis exists.

De-escalation is a router-internal transition: it carries no trigger, so it is
**not** emitted through the contract's ``escalation_propose`` action (which
requires a trigger). It is observable via :meth:`next_model` returning the cheap
model again, and via the returned :class:`EscalationDecision`.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field

from .client import EscalationClient
from .config import ThresholdConfig
from .triggers import TurnSignals, detect

#: Hook invoked with each *escalate*-edge decision (so a harness can emit the
#: EscalationProposed event). De-escalation edges are not passed — they have no
#: trigger to propose.
OnEscalation = Callable[["EscalationDecision"], None]


@dataclass(frozen=True, slots=True)
class EscalationDecision:
    """One router state transition.

    Attributes:
        edge: ``"escalate"`` or ``"de_escalate"``.
        trigger: The fired trigger kind on an escalate edge; ``None`` on a
            de-escalation edge.
        from_model: The model handing off.
        to_model: The model the next turn is proposed to run on.
        session_id: The orchestrator session.
        turn_index: 0-based turn that produced the transition.
        state_before: Router state before the edge.
        state_after: Router state after the edge (``escalated`` /
            ``de_escalated``).
        trigger_detail: Evidence from the detector (escalate only).
        fired_threshold: The threshold that fired (escalate only).
    """

    edge: str
    trigger: str | None
    from_model: str
    to_model: str
    session_id: str
    turn_index: int
    state_before: str
    state_after: str
    trigger_detail: str | None = None
    fired_threshold: float | None = None


@dataclass(slots=True)
class EscalationRouter:
    """Per-session cheap/strong router with de-escalation hysteresis.

    Attributes:
        cheap_model: Identifier of the cheap orchestrator model.
        strong_model: Identifier of the strong orchestrator model.
        config: Effective per-trigger thresholds (also supplies K).
        session_id: The orchestrator session id (stamped on every decision).
        on_escalation: Optional hook fired with each escalate-edge decision.
    """

    cheap_model: str
    strong_model: str
    config: ThresholdConfig
    session_id: str
    on_escalation: OnEscalation | None = None
    _state: str = field(default="cheap", init=False)
    _clean_streak: int = field(default=0, init=False)
    _turn_index: int = field(default=0, init=False)

    @property
    def state(self) -> str:
        """The current resting state: ``cheap`` or ``escalated``."""
        return self._state

    @property
    def clean_streak(self) -> int:
        """Consecutive clean (no-trigger) turns observed while escalated."""
        return self._clean_streak

    @property
    def turn_index(self) -> int:
        """Number of turns observed so far (the next turn's 0-based index)."""
        return self._turn_index

    def next_model(self) -> str:
        """Return the model that should drive the next turn."""
        return self.strong_model if self._state == "escalated" else self.cheap_model

    def observe(self, signals: TurnSignals) -> EscalationDecision | None:
        """Feed one turn's signals; maybe transition state.

        Args:
            signals: What the harness observed during the turn just completed.

        Returns:
            The :class:`EscalationDecision` when this turn caused a transition
            (escalate or de-escalate), else ``None``. On an escalate edge the
            :attr:`on_escalation` hook is also invoked before returning.
        """
        idx = self._turn_index
        self._turn_index += 1
        fired = detect(signals, self.config)

        if self._state == "cheap":
            if fired is None:
                return None
            decision = EscalationDecision(
                edge="escalate",
                trigger=fired.trigger_kind,
                from_model=self.cheap_model,
                to_model=self.strong_model,
                session_id=self.session_id,
                turn_index=idx,
                state_before="cheap",
                state_after="escalated",
                trigger_detail=fired.detail,
                fired_threshold=fired.threshold_value,
            )
            self._state = "escalated"
            self._clean_streak = 0
            if self.on_escalation is not None:
                self.on_escalation(decision)
            return decision

        # state == "escalated"
        if fired is not None:
            # A trigger while escalated resets the streak; the strong tier keeps
            # driving and we do NOT re-emit (already escalated).
            self._clean_streak = 0
            return None

        self._clean_streak += 1
        if self._clean_streak < self.config.de_escalation_turns:
            return None

        # K consecutive clean turns: de-escalate back to the cheap tier.
        self._state = "cheap"
        self._clean_streak = 0
        return EscalationDecision(
            edge="de_escalate",
            trigger=None,
            from_model=self.strong_model,
            to_model=self.cheap_model,
            session_id=self.session_id,
            turn_index=idx,
            state_before="escalated",
            state_after="de_escalated",
        )


def make_emitter(
    client: EscalationClient,
    *,
    project_id: str | None = None,
    default_reason: str | None = None,
) -> OnEscalation:
    """Build an :data:`OnEscalation` hook that emits via an :class:`EscalationClient`.

    Wire it into :class:`EscalationRouter` so each escalate edge lands an
    ``EscalationProposed`` event:

        router = EscalationRouter(cheap, strong, config, session_id=sid,
                                  on_escalation=make_emitter(client, project_id="proj"))

    Args:
        client: The toolkit-server client to emit through.
        project_id: Optional project scope for the event entity.
        default_reason: Optional fixed rationale; when ``None`` a reason is
            synthesised from the trigger detail.

    Returns:
        A hook suitable for :attr:`EscalationRouter.on_escalation`.
    """

    def _emit(decision: EscalationDecision) -> None:
        if decision.trigger is None:  # defensive: only escalate edges are emitted
            return
        reason = default_reason or f"{decision.trigger} fired: {decision.trigger_detail}"
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
            project_id=project_id,
            reason=reason,
        )

    return _emit
