// Package measure provides MCP action handlers for the measure meta-tool.
package measure

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/mcpparam"
	"toolkit/internal/obs"
	"toolkit/internal/qwenctx"
	"toolkit/internal/rubric"
)

// ClassifyDeps holds the shared dependencies for classify handlers.
type ClassifyDeps struct {
	Pool    *db.Pool
	Router  *router.Router
	Rubrics *rubric.Registry
	Project string
	// VaultRoot overrides the default vault path ($HOME/.claude/vault) used by
	// team-context derivation. Empty string uses the default; tests inject a
	// temp dir to keep the scan hermetic.
	VaultRoot string
}

// ClassifyResponse is the unified response shape for every classify_* action.
// Fields populate based on success path or which rubric ran:
//   - Success path: OK, Label, LatencyMS, ModelName
//   - Chain-proportionality adds TeamContextProse
//   - Param-missing / unsupported errors set Error and leave success fields empty
//
// omitempty makes the JSON output identical to the prior map[string]any shape.
type ClassifyResponse struct {
	OK               bool   `json:"ok,omitempty"`
	Label            string `json:"label,omitempty"`
	LatencyMS        int64  `json:"latency_ms,omitempty"`
	ModelName        string `json:"model_name,omitempty"`
	TeamContextProse string `json:"team_context_prose,omitempty"`
	Error            string `json:"error,omitempty"`
}

// classify runs inference against the named rubric and returns a typed
// ClassifyResponse. Records benchmark telemetry and handles unclassifiable
// responses uniformly. Returns an error for inference failures or unparseable
// responses (NoMatch / MultipleLabels).
//
// unclassifiableFallback is the label substituted when the model says
// "unclassifiable" — each rubric has a different catch-all convention.
//
// Handlers may set per-action extras on the returned struct (e.g.
// TeamContextProse for chain-assessment).
func classify(
	ctx context.Context,
	deps ClassifyDeps,
	rubricName string,
	inputText string,
	unclassifiableFallback string,
) (ClassifyResponse, error) {
	def, ok := deps.Rubrics.Get(rubricName)
	if !ok {
		return ClassifyResponse{}, fmt.Errorf("rubric %q not found in registry", rubricName)
	}
	if !def.IsDeployed {
		return ClassifyResponse{}, fmt.Errorf("rubric %q is not deployed", rubricName)
	}

	system, user := rubric.ComposeClassify(def, inputText)

	// Stamp the task_id (= rubric name) so the universal inference_invocations
	// row attributes this call to its rubric on /inference (bug 1328).
	ctx = qwenctx.WithTaskID(ctx, rubricName)
	start := time.Now()
	genResult, err := deps.Router.Generate(ctx, user, system)
	latencyMS := time.Since(start).Milliseconds()

	if err != nil {
		dbResult := db.ClassifyResult{
			RawResponse:  err.Error(),
			LatencyMS:    latencyMS,
			InvocationOK: false,
		}
		if recordErr := db.RecordBenchmarkDispatch(ctx, deps.Pool, deps.Project, rubricName, deps.Router.ModelName(), dbResult); recordErr != nil {
			obs.Logger(ctx).Warn("classify: record benchmark failed",
				slog.String("rubric", rubricName),
				slog.String("err", recordErr.Error()),
			)
		}
		return ClassifyResponse{}, fmt.Errorf("inference_router error: %w", err)
	}

	rawResponse := genResult.Text
	parsed := rubric.ParseSingleClass(rawResponse, def.OutputEnum)
	label, parseErr := labelFromParsed(parsed, unclassifiableFallback, rawResponse)

	// Record telemetry whether parse succeeded or not. invocation_ok reflects
	// whether the inference call returned text (it did); parse-side failures
	// are surfaced via the returned error so callers can distinguish "model
	// said something we couldn't use" from "model didn't respond at all."
	dbResult := db.ClassifyResult{
		Label:        label, // empty when parseErr is non-nil
		RawResponse:  rawResponse,
		LatencyMS:    latencyMS,
		InputTokens:  genResult.InputTokens,
		OutputTokens: genResult.OutputTokens,
		InvocationOK: parseErr == nil,
	}
	if recordErr := db.RecordBenchmarkDispatch(ctx, deps.Pool, deps.Project, rubricName, deps.Router.ModelName(), dbResult); recordErr != nil {
		obs.Logger(ctx).Warn("classify: record benchmark failed",
			slog.String("rubric", rubricName),
			slog.String("err", recordErr.Error()),
		)
	}

	if parseErr != nil {
		return ClassifyResponse{}, parseErr
	}

	return ClassifyResponse{
		OK:        true,
		Label:     label,
		LatencyMS: latencyMS,
		ModelName: deps.Router.ModelName(),
	}, nil
}

// labelFromParsed converts a ParseResult to a final label or a typed error.
// ParsedSingle and ParsedUnclassifiable are success paths. ParsedNone and
// ParsedMultiple return errors so callers can distinguish "model gave usable
// output" from "model said something off-rubric or ambiguous."
//
// Error messages mirror Rust RubricError::NoMatch / MultipleLabelsUnderSingleClass.
// rawResponse is included in the NoMatch message so the dashboard / logs can
// see what the model actually said (matches Rust's `{ raw }` field).
func labelFromParsed(p rubric.ParseResult, unclassifiableFallback, rawResponse string) (string, error) {
	switch p.Kind {
	case rubric.ParsedSingle:
		return p.Label, nil
	case rubric.ParsedUnclassifiable:
		return unclassifiableFallback, nil
	case rubric.ParsedMultiple:
		return "", fmt.Errorf("classify response returned multiple labels under SingleClass: %v", p.Labels)
	default: // ParsedNone
		return "", fmt.Errorf("classify response did not match any allowed label or 'unclassifiable': %s", rawResponse)
	}
}

// errResponse wraps a missing-param error into a structured envelope. Returns
// (ClassifyResponse{Error: msg}, nil) so the dispatcher serialises the envelope
// as the success payload (matching Rust handler shape).
func errResponse(msg string) (ClassifyResponse, error) {
	return ClassifyResponse{Error: msg}, nil
}

// HandleClassifyChainTaskProportionality classifies a chain task's
// proportionality against the team's bandwidth and prior signal strength.
//
// Required param: task_spec (the task's problem_statement + acceptance_criteria).
// Optional param: team_context_override — skips automatic derivation and uses
// the caller-supplied prose verbatim.
//
// Response includes team_context_prose so callers can inspect the grounding
// the dispatcher actually used (mirrors Rust handler shape).
func HandleClassifyChainTaskProportionality(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	taskSpec := mcpparam.String(params, "task_spec")
	if taskSpec == "" {
		return errResponse("params.task_spec is required")
	}

	teamContextProse := mcpparam.String(params, "team_context_override")
	if teamContextProse == "" {
		tc, err := DeriveTeamContext(ctx, deps.Pool, deps.VaultRoot, deps.Project, taskSpec)
		if err != nil {
			return ClassifyResponse{}, fmt.Errorf("derive team context: %w", err)
		}
		teamContextProse = tc.Prose()
	}

	composed := fmt.Sprintf("%s\n\nTeam context:\n%s", strings.TrimSpace(taskSpec), teamContextProse)
	result, err := classify(ctx, deps, "chain-assessment", composed, "unclear")
	if err != nil {
		return ClassifyResponse{}, err
	}
	result.TeamContextProse = teamContextProse
	return result, nil
}

// HandleClassifyRetirementObservation classifies an observation describing
// recent project activity for a retirement signal.
//
// Required param: observation_text.
func HandleClassifyRetirementObservation(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "observation_text")
	if text == "" {
		return errResponse("params.observation_text is required")
	}
	return classify(ctx, deps, "retirement-signal", text, "not-retirement")
}

// HandleClassifyArtifactTier classifies an artifact descriptor into the
// session-loading tier at which it should be loaded.
//
// Required param: artifact_descriptor.
func HandleClassifyArtifactTier(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "artifact_descriptor")
	if text == "" {
		return errResponse("params.artifact_descriptor is required")
	}
	return classify(ctx, deps, "tiered-context", text, "unclassifiable")
}

// HandleClassifyAuditFindingSeverity classifies one agentic-architecture
// audit finding by severity.
//
// Required param: finding_prose.
func HandleClassifyAuditFindingSeverity(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "finding_prose")
	if text == "" {
		return errResponse("params.finding_prose is required")
	}
	return classify(ctx, deps, "agentic-audit", text, "unclassifiable")
}

// HandleClassifyArtifactReviewCriterion evaluates one review criterion against
// one artifact excerpt.
//
// Required params: artifact_excerpt, purpose, criterion.
func HandleClassifyArtifactReviewCriterion(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	excerpt := mcpparam.String(params, "artifact_excerpt")
	purpose := mcpparam.String(params, "purpose")
	criterion := mcpparam.String(params, "criterion")

	if excerpt == "" {
		return errResponse("params.artifact_excerpt is required")
	}
	if purpose == "" {
		return errResponse("params.purpose is required")
	}
	if criterion == "" {
		return errResponse("params.criterion is required")
	}
	composed := fmt.Sprintf("Purpose: %s. Criterion: %q\n\n%s", purpose, criterion, excerpt)
	return classify(ctx, deps, "artifact-review", composed, "unclassifiable")
}

// HandleClassifySessionRoutingTrigger classifies a user input by its
// session-routing dispatch trigger.
//
// Single Qwen pass. This handler used to escalate to the remote Claude model
// whenever Qwen said "no-trigger" but the input contained role-domain
// vocabulary — a compensation for Qwen under-detecting role invocations. The
// role system was retired 2026-07-22 (there is no role_load to dispatch to),
// so the escalation had nothing left to rescue and went with it.
//
// Required param: user_input.
func HandleClassifySessionRoutingTrigger(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	userInput := mcpparam.String(params, "user_input")
	if userInput == "" {
		return errResponse("params.user_input is required")
	}

	def, ok := deps.Rubrics.Get("session-routing")
	if !ok {
		return ClassifyResponse{}, fmt.Errorf("session-routing rubric not found in registry")
	}

	system, user := rubric.ComposeClassify(def, userInput)
	ctx = qwenctx.WithTaskID(ctx, "session-routing")
	start := time.Now()
	genResult, err := deps.Router.Generate(ctx, user, system)
	latencyMS := time.Since(start).Milliseconds()

	if err != nil {
		dbResult := db.ClassifyResult{RawResponse: err.Error(), LatencyMS: latencyMS, InvocationOK: false}
		if e := db.RecordBenchmarkDispatch(ctx, deps.Pool, deps.Project, "session-routing", deps.Router.ModelName(), dbResult); e != nil {
			obs.Logger(ctx).Warn("classify session-routing: record failed",
				slog.String("err", e.Error()))
		}
		return ClassifyResponse{}, fmt.Errorf("inference_router error: %w", err)
	}

	rawResponse := genResult.Text
	parsed := rubric.ParseSingleClass(rawResponse, def.OutputEnum)
	qwenLabel, parseErr := labelFromParsed(parsed, "no-trigger", rawResponse)
	if parseErr != nil {
		// Qwen returned something we can't map cleanly. Record the failure
		// and propagate — matches Rust's RubricError::NoMatch / Multiple paths.
		dbResult := db.ClassifyResult{
			RawResponse:  rawResponse,
			LatencyMS:    latencyMS,
			InputTokens:  genResult.InputTokens,
			OutputTokens: genResult.OutputTokens,
			InvocationOK: false,
		}
		if e := db.RecordBenchmarkDispatch(ctx, deps.Pool, deps.Project, "session-routing", deps.Router.ModelName(), dbResult); e != nil {
			obs.Logger(ctx).Warn("classify session-routing: record failed",
				slog.String("err", e.Error()))
		}
		return ClassifyResponse{}, parseErr
	}

	label := qwenLabel

	notesPayload, _ := json.Marshal(sessionRoutingNotes{
		Raw:        rawResponse,
		FinalLabel: label,
	})
	dbResult := db.ClassifyResult{
		Label:         label,
		RawResponse:   rawResponse,
		LatencyMS:     latencyMS,
		InputTokens:   genResult.InputTokens,
		OutputTokens:  genResult.OutputTokens,
		InvocationOK:  true,
		NotesOverride: string(notesPayload),
	}
	if e := db.RecordBenchmarkDispatch(ctx, deps.Pool, deps.Project, "session-routing", deps.Router.ModelName(), dbResult); e != nil {
		obs.Logger(ctx).Warn("classify session-routing: record failed",
			slog.String("err", e.Error()))
	}

	return ClassifyResponse{
		OK:        true,
		Label:     label,
		LatencyMS: latencyMS,
		ModelName: deps.Router.ModelName(),
	}, nil
}

// sessionRoutingNotes is the typed payload serialised into the notes_override
// column for session-routing benchmark rows. Keeping it typed makes the JSON
// produced for telemetry self-documenting and lint-clean.
type sessionRoutingNotes struct {
	Raw        string `json:"raw"`
	FinalLabel string `json:"final_label"`
}

// HandleClassifyPreCommitFailure classifies pre-commit hook stderr by its
// dominant failure cause.
//
// Required param: stderr.
func HandleClassifyPreCommitFailure(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "stderr")
	if text == "" {
		return errResponse("params.stderr is required")
	}
	return classify(ctx, deps, "pre-commit-failure", text, "unclassifiable")
}

// HandleClassifyDocstringDrift classifies whether a function's docstring has
// drifted from its current body.
//
// Required param: function_snippet.
func HandleClassifyDocstringDrift(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "function_snippet")
	if text == "" {
		return errResponse("params.function_snippet is required")
	}
	return classify(ctx, deps, "docstring-drift", text, "unclassifiable")
}

// HandleClassifyBugSeverity classifies a filed bug report's severity along
// the two-axis observer-impact × blast-radius matrix from
// skill:bug-filing-discipline. Output is one of {low, medium, high, unclear}.
//
// Required param: bug_report (the bug's title + problem_statement
// concatenated, or any prose describing the bug's symptom + scope).
func HandleClassifyBugSeverity(ctx context.Context, deps ClassifyDeps, params json.RawMessage) (ClassifyResponse, error) {
	text := mcpparam.String(params, "bug_report")
	if text == "" {
		return errResponse("params.bug_report is required")
	}
	return classify(ctx, deps, "bug-severity", text, "unclear")
}
