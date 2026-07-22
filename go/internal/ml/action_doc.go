package ml

// action_doc.go is the descriptor-registry seam for the ml surface's action docs
// (chain migrate-ml-action-docs-to-derive-contract — the per-surface instantiation
// of the contract established on work, docs/ACTION_DOC_CONTRACT.md). It is the
// single source of the ml surface's action docs: each param's TYPE is DERIVED from
// the handler's typed param struct, and only the irreducible semantics (purpose,
// param name-list/order/required/description, errors, notes, returns) are authored
// in a co-located Go descriptor.
//
// ml is the SIMPLEST surface to migrate: it has exactly one statically-registered
// action — `inference` — and that action is STRUCT-BACKED (HandleInference
// json.Unmarshals into InferenceParams), so every documented param derives its
// type from the struct (ParamStruct set, authored Type left empty — gate-enforced
// by TestMLRegistryDerivedParamsHaveEmptyAuthoredType). Unlike knowledge, ml has
// NO map-bound actions, so there is no mcpparam binder-parity gate to add.
//
// Per-task CONVENIENCE actions (route_query, curation_score, …) register on the ml
// table dynamically at model-promotion time (see table.go RegisterConvenience).
// None is promoted today, and they carry no co-located descriptor, so the corpus
// this registry generates covers `inference` only — matching the static dispatch
// table. The surface-wide _general.toml chunk stays hand-authored cross-cutting
// prose (the corpus generator exempts every <surface>/_general.toml).
//
// DECIDED action-doc path for convenience actions (suggestion 39, ratified in chain
// finalize-action-docs-epic; see docs/ACTION_DOC_CONTRACT.md). A promoted-model
// convenience action would otherwise be dispatchable as ml.<action> but invisible
// to the action-doc system: not in mlActionRegistry, so no generated corpus chunk,
// admin.action_describe(ml, <action>) returns a MISS, and the no-diff/orphan gates
// (build-time registry ↔ on-disk corpus) can't see it. The path is option (i): a
// convenience-action-shipping chain MUST add a co-located descriptor + an
// mlActionRegistry entry at BUILD time, so the action generates + gates exactly like
// `inference` — NOT a speculative runtime-doc-registration machinery. Nothing is
// broken today (no convenience action is registered); this is recorded so the next
// model-promotion chain wires the descriptor before the first one ships.
//
// HISTORY / framing: the chain spec framed ml as having an EMPTY corpus. It did
// not — corpus/ml/inference.toml shipped hand-authored (chain
// single-source-action-describe T6, commit 19c0c829), documenting its params,
// returns, and errors as PROSE inside `notes` rather than as structured blocks.
// This migration is therefore a knowledge-style MIGRATE that additionally
// RESTRUCTURES that prose into struct-derived [[params]] + structured [[errors]] +
// a [returns] block — an intended, reviewed output change pinned by the T1 net and
// re-baselined at the T3 flip. (Filed: bug
// ml-action-doc-migration-spec-wrongly-says-corpus-empty.)
//
// The generated corpus + admin.action_describe(ml, X) derive from this registry
// via MLActionSpecs(); byte-parity is pinned by the T1 characterization net
// (internal/actiondocs/surface_contract_net_test.go).

import (
	"reflect"

	"toolkit/internal/actionspec"
)

// inferenceDoc is the co-located descriptor for ml.inference. Param TYPES derive
// from InferenceParams (ParamStruct set in the registry below) — every DocParam
// here leaves Type empty. model_id / grounding_event_id derive `int64`
// (→ integer); task derives `string` (optional → optional_string); features_data
// ([]float32) and features_shape ([]int64) derive `object[]` — their numeric
// element shape is documented in the description, per the contract's
// "sub-shape lives in the description, not a sibling param" rule.
var inferenceDoc = actionspec.ActionDoc{
	Purpose: "Run inference against a trained_model. Accepts either model_id (specific version) or task (resolves the currently-promoted row). Writes a model_predictions telemetry row keyed by span_id + features_hash.",
	Params: []actionspec.DocParam{
		{Name: "model_id", Required: false, Description: "trained_models.id — serve this exact version (useful for the A/B harness when comparing exact versions). Wins when both model_id and task are supplied."},
		{Name: "task", Required: false, Description: "Task identifier (kebab-case); resolves to the trained_models row where status='promoted'. Resolution is project-scoped, so a task lookup requires project at the dispatch envelope."},
		{Name: "features_data", Required: true, Description: "Flattened input tensor ([]float32), row-major. len(features_data) must equal the product of features_shape."},
		{Name: "features_shape", Required: true, Description: "Tensor shape ([]int64) declaring the dims of features_data."},
		{Name: "grounding_event_id", Required: false, Description: "grounding_events.id when this inference resolves a search-triggered call (cross-encoder reranker, source router); populates model_predictions.grounding_event_id for downstream join-against-clicks projections."},
	},
	Errors: []actionspec.ActionError{
		{Condition: "neither model_id nor task supplied", Message: "ml.inference requires either model_id or task"},
		{Condition: "task supplied without project at the dispatch envelope", Message: "ml.inference requires project when resolving by task; pass project at the dispatch envelope"},
		{Condition: "no promoted row for the task, or no row with the supplied model_id", Message: "ml: trained model not found (ErrModelNotFound)"},
		{Condition: "the resolved row is in a gated lifecycle state (training / retired)", Message: "ml: trained model is in a gated lifecycle state (training/retired) (ErrModelGated)"},
		{Condition: "features_data length does not equal the product of features_shape (or either is empty, or a dim is non-positive)", Message: "ml: input features don't match expected shape (ErrInputShape)"},
	},
	Returns: &actionspec.ActionReturn{
		Shape:       "InferenceResult",
		Description: "On success: prediction.output ([]float32) + prediction.output_shape ([]int64); prediction.latency_ms; prediction.model_id (the trained_model.id served); prediction.feat_hash (SHA-256 of canonical-serialized features); span_id (the agent-first-substrate span the model_predictions row was written under); prediction_row_id (model_predictions.id). Caller-controlled errors return as a JSON envelope (error + hint), not a Go error.",
	},
	Notes: "MODEL RESOLUTION: model_id wins when both are supplied. The status='promoted' filter is the only loadable lookup through `task`; `evaluating` / `ab_testing` rows reach the serving layer only via model_id.\n\nTELEMETRY: every successful inference writes one model_predictions row, content-keyed by features_hash (SHA-256 of canonical-serialized features) and spanned via span_id from the agent-first-substrate envelope.\n\nDEGRADED MODE: when ML_MODELS_ROOT isn't configured or libonnxruntime.so isn't loadable (scripts/setup-ml-deps.sh not run on this box), inference returns a typed error envelope rather than panicking.\n\nSURFACE BOUNDARY: ml inference happens here; trained_model lifecycle (forge / list / promote / retire) lives on the work surface (work.trained_model_*).",
}

// mlActionRegistry is the ordered, co-located descriptor registry — the single
// source of the ml surface's action docs. MLActionSpecs() derives the catalog the
// corpus generator + admin.action_describe consume. The T1 characterization net
// (internal/actiondocs/surface_contract_net_test.go) is the byte-parity oracle.
// ParamStruct is set (inference is struct-backed), so its param Types derive from
// InferenceParams and the authored Types stay empty.
var mlActionRegistry = []actionspec.ActionEntry{
	{Name: "inference", Doc: inferenceDoc, ParamStruct: reflect.TypeOf(InferenceParams{})},
}

// MLActionSpecs returns the ml surface's full action catalog, derived from the
// co-located descriptor registry. Each param's type is derived from
// InferenceParams. This is what the corpus generator projects into
// corpus/ml/*.toml and what admin.action_describe(ml, X) serves once the corpus is
// generated (T3).
func MLActionSpecs() []actionspec.ActionSpec {
	return actionspec.DeriveSpecs(mlActionRegistry)
}
