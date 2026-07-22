"""toolkit-server MCP client for the escalation contract.

A thin client over the HTTP ``POST /mcp/admin`` route (the same dispatch table
the stdio MCP serves). Standard-library only. Three calls:

* :meth:`EscalationClient.list_thresholds` — read the effective per-trigger config.
* :meth:`EscalationClient.set_threshold` — upsert one threshold row.
* :meth:`EscalationClient.propose` — emit an ``EscalationProposed`` event.

The HTTP path stamps the substrate's ``system`` actor (actor is transport-
inferred), so the rationale gate does not apply; the proposal's ``reason`` is
still recorded as the event rationale. See ``docs/ORCHESTRATOR_ESCALATION.md``
§3.1 / §7.3.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from collections.abc import Callable
from typing import Any

from .config import ThresholdConfig

#: Transport seam: ``(path, body) -> decoded JSON``. The default hits the live
#: server over HTTP; tests inject a fake to assert request shape and stub
#: responses without a running server.
Transport = Callable[[str, dict[str, Any]], Any]


class EscalationError(RuntimeError):
    """Raised when a toolkit-server call fails at the transport or contract level."""


class EscalationClient:
    """Client for the escalation admin actions on a toolkit-server HTTP daemon."""

    def __init__(
        self,
        base_url: str,
        *,
        project: str | None = None,
        timeout: float = 10.0,
        transport: Transport | None = None,
    ) -> None:
        """Args:
        base_url: Daemon root, e.g. ``http://localhost:3000``.
        project: Optional project scope sent as the call envelope ``project``.
        timeout: Per-request timeout (seconds) for the default HTTP transport.
        transport: Override the HTTP transport (tests inject a fake).
        """
        self._base = base_url.rstrip("/")
        self._project = project
        self._timeout = timeout
        self._transport = transport or self._http_transport

    def _http_transport(self, path: str, body: dict[str, Any]) -> Any:
        data = json.dumps(body).encode("utf-8")
        req = urllib.request.Request(
            self._base + path,
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:  # noqa: S310 (trusted localhost daemon)
                raw = resp.read()
        except urllib.error.URLError as exc:
            raise EscalationError(f"POST {path} failed: {exc}") from exc
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise EscalationError(f"POST {path} returned non-JSON: {raw!r}") from exc

    def _admin(self, action: str, params: dict[str, Any], *, rationale: str | None = None) -> Any:
        body: dict[str, Any] = {"action": action, "params": params}
        if rationale:
            body["rationale"] = rationale
        if self._project:
            body["project"] = self._project
        resp = self._transport("/mcp/admin", body)
        if isinstance(resp, dict) and "error" in resp:
            raise EscalationError(f"{action} rejected: {resp}")
        return resp

    def list_thresholds(self, project_id: str = "") -> ThresholdConfig:
        """Read the effective per-trigger config (global defaults + overrides).

        Args:
            project_id: Project whose overrides to overlay; ``""`` reads globals.

        Returns:
            The merged :class:`ThresholdConfig`.
        """
        resp = self._admin("escalation_threshold_list", {"project_id": project_id})
        if not isinstance(resp, list):
            raise EscalationError(f"escalation_threshold_list returned non-list: {resp!r}")
        return ThresholdConfig.from_rows(resp)

    def set_threshold(
        self,
        trigger_kind: str,
        threshold_value: float,
        *,
        project_id: str = "",
        enabled: bool | None = None,
        de_escalation_turns: int | None = None,
        rationale: str,
    ) -> dict[str, Any]:
        """Upsert one ``(project_id, trigger_kind)`` threshold row.

        Args:
            trigger_kind: One of the five closed trigger kinds.
            threshold_value: The trigger's threshold (count or confidence floor).
            project_id: Row scope; ``""`` writes a global-default row.
            enabled: Optional; omitted preserves the existing value.
            de_escalation_turns: Optional hysteresis K; omitted preserves it.
            rationale: Required by the dispatch rationale gate for agent actors.

        Returns:
            The handler result dict (``ok``, echoed row fields).
        """
        params: dict[str, Any] = {
            "project_id": project_id,
            "trigger_kind": trigger_kind,
            "threshold_value": threshold_value,
        }
        if enabled is not None:
            params["enabled"] = enabled
        if de_escalation_turns is not None:
            params["de_escalation_turns"] = de_escalation_turns
        resp = self._admin("escalation_threshold_set", params, rationale=rationale)
        if not isinstance(resp, dict):
            raise EscalationError(f"escalation_threshold_set returned non-object: {resp!r}")
        return resp

    def propose(
        self,
        *,
        trigger: str,
        from_model: str,
        to_model: str,
        session_id: str,
        state_before: str,
        state_after: str,
        turn_index: int = 0,
        trigger_detail: str | None = None,
        fired_threshold: float | None = None,
        project_id: str | None = None,
        reason: str | None = None,
    ) -> str:
        """Emit one ``EscalationProposed`` event; return its ``event_id``.

        Args:
            trigger: Which trigger fired (one of the five kinds).
            from_model: The model proposing the handoff.
            to_model: The model the next turn is proposed to run on.
            session_id: The orchestrator session (the event entity slug).
            state_before: Router state before the transition.
            state_after: Router state after the transition.
            turn_index: 0-based turn that produced the proposal.
            trigger_detail: Optional evidence string.
            fired_threshold: Optional snapshot of the threshold that fired.
            project_id: Optional project scope for the event entity.
            reason: Optional rationale recorded on the event envelope.

        Returns:
            The emitted event's ``event_id``.
        """
        params: dict[str, Any] = {
            "trigger": trigger,
            "from_model": from_model,
            "to_model": to_model,
            "session_id": session_id,
            "turn_index": turn_index,
            "state_before": state_before,
            "state_after": state_after,
        }
        if trigger_detail is not None:
            params["trigger_detail"] = trigger_detail
        if fired_threshold is not None:
            params["fired_threshold"] = fired_threshold
        if project_id is not None:
            params["project_id"] = project_id
        if reason is not None:
            params["reason"] = reason
        resp = self._admin("escalation_propose", params, rationale=reason)
        if not isinstance(resp, dict) or "event_id" not in resp:
            raise EscalationError(f"escalation_propose returned no event_id: {resp!r}")
        return str(resp["event_id"])
