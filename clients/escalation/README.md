# escalation — orchestrator-tier escalation contract (reference library)

Harness-agnostic reference implementation of the orchestrator-tier escalation
contract. A cheap orchestrator-tier model (DeepSeek V4 Pro, Qwen) drives the
conversational loop; when it hits trouble it can't recover from cheaply, the
contract proposes handing the **next turn** to a strong tier (Opus 4.7), records
the proposal as an `EscalationProposed` event through toolkit-server, and — after
the strong tier has stabilised the work — hands control back.

Full design: [`docs/ORCHESTRATOR_ESCALATION.md`](../../docs/ORCHESTRATOR_ESCALATION.md)
in the mcp-servers repo.

**Standard-library only** — no third-party runtime dependency, so any harness can
vendor or `pip install` it without pulling a transitive tree.

## The two-call harness contract

```python
from escalation import EscalationClient, EscalationRouter, TurnSignals, make_emitter

client = EscalationClient("http://localhost:3000", project="mcp-servers")
config = client.list_thresholds("mcp-servers")          # effective per-trigger config

router = EscalationRouter(
    cheap_model="deepseek-v4-pro",
    strong_model="claude-opus-4-7",
    config=config,
    session_id="orch-7f3a9c2e",
    on_escalation=make_emitter(client, project_id="mcp-servers"),  # emits EscalationProposed
)

# Each turn:
model = router.next_model()                # read at the top of the turn
...                                         # run the turn on `model`
router.observe(TurnSignals(retries_used=2)) # feed signals after the turn
```

- `next_model()` returns the model that should drive the next turn (`cheap` until
  a trigger fires, then `strong` until K clean turns de-escalate).
- `observe(signals)` feeds the turn's observed signals through the detectors;
  a fired trigger escalates (and emits the event via the `on_escalation` hook).

## The five triggers

`retry_exhaustion`, `low_confidence`, `repeated_tool_error`, `parse_failure`,
`explicit_handoff` — see the design doc §2. Thresholds are tunable per project
through the `escalation_threshold_set` admin action (or `client.set_threshold`).

## De-escalation hysteresis

After escalating, the router stays on the strong tier until **K consecutive clean
turns** (`de_escalation_turns`, default 2) — preventing flap on borderline work.

## Modules

| Module | Role |
|---|---|
| `escalation.config` | `TriggerThreshold` / `ThresholdConfig`, parsed from `escalation_threshold_list`. |
| `escalation.triggers` | Pure per-trigger detectors over `TurnSignals`. |
| `escalation.client` | `EscalationClient` over `POST /mcp/admin`. |
| `escalation.router` | `EscalationRouter` state machine + `make_emitter` bridge. |

## Tests

```bash
# Unit tests (no server needed):
PYTHONPATH=src pytest tests/ -k "not integration"

# Gates:
ruff check . && ruff format --check . && mypy --strict src tests

# End-to-end (against a live, isolated worktree-mcp daemon):
PORT=3994 ./scripts/worktree-mcp.sh --fresh &       # from the repo root
TOOLKIT_ESCALATION_BASE_URL=http://localhost:3994 \
  TOOLKIT_ESCALATION_DB=/tmp/toolkit-worktree-orchestrator-tier-escalation-contract.db \
  PYTHONPATH=src pytest tests/test_integration.py -v
```
