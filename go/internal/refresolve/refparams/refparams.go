// Package refparams holds the typed param structs for the refresolve-backed
// knowledge-surface actions (parse_context, resolve_references) in a dependency-
// light leaf package, so the knowledge action-doc registry can reflect them for
// type derivation WITHOUT package knowledge importing package refresolve (which
// imports knowledge — that edge would cycle). Mirrors internal/arcreview/arcparams,
// which the work registry uses for the same reason (chain
// migrate-knowledge-action-docs-to-derive-contract; precedent
// establish-action-doc-contract-on-work T3).
//
// This package imports nothing internal — it is a pure leaf. Package refresolve
// type-aliases ParseContextParams so its handlers keep their existing names.
package refparams

// ParseContextParams is the typed request shape for both the parse_context
// action (canonical) and resolve_references (alias). SessionID +
// CachePolicyOverride are reserved for the filter-cache layer; the canonical-
// name CI gate (go/internal/actiondocs/param_tag_gate_test.go) requires every
// documented param to have a reachable binding, so the tags ship with the shell.
type ParseContextParams struct {
	MessageText         string `json:"message_text"`
	SessionID           string `json:"session_id,omitempty"`
	TopKPerShape        int    `json:"top_k_per_shape,omitempty"`
	IncludeNoHits       bool   `json:"include_no_hits,omitempty"`
	TotalBudgetMs       int64  `json:"total_budget_ms,omitempty"`
	CachePolicyOverride string `json:"cache_policy_override,omitempty"`
	// InlineSkillBodies overrides the TOOLKIT_PARSE_CONTEXT_INLINE_BODIES env
	// var for one call (chain 602). Nil → use env-var setting; non-nil → use
	// this value. Tests use this to exercise both states without touching the
	// process env.
	InlineSkillBodies *bool `json:"inline_skill_bodies,omitempty"`
	// InlineBudgetBytes overrides the default envelope budget for inlined skill
	// bodies (default 20480 / 20 KB). Zero → use default. The per-skill cap
	// (8 KB) is not caller-tunable.
	InlineBudgetBytes int `json:"inline_budget_bytes,omitempty"`
	// DisciplineFiring selects who owns the intent-discipline firing policy.
	// "" / "server" (default): parse_context applies the noise-budget + recent-
	// fire suppression itself and surfaces the budgeted disciplines inline in
	// References — the Claude Code path. "client": parse_context returns the raw
	// applicable disciplines in CandidateDisciplines (pre-budget) and surfaces
	// NONE inline, so a client that owns the firing policy (corpos, chain
	// toolkit-decomposition T5) applies its own budget. Detect/map stays here;
	// only the firing cadence moves.
	DisciplineFiring string `json:"discipline_firing,omitempty"`
}
