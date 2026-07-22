// Package refresolve owns the reference-detection layer of the
// reference-resolution substrate — scans a user message and emits
// tagged Reference structs, one per detected token of a recognized
// shape (chain_slug, task_slug, bug_slug, path, skill_name,
// project_name, tool_name, forge_schema, library_entry, domain_term,
// external_technical).
//
// ## Intended use
//
// **Workflow served:** when the user names a specific token an
// agent can't bind to current context, the agent calls the
// resolve_references MCP action (T4); the action's handler invokes
// Detect to tag the message's reference-shape tokens and Dispatch
// to resolve each one. Detection is shape-based, not
// uncertainty-based — LLMs are unreliable at "do I know X?" but
// reliable at "is this token a slug-shaped reference I don't have
// loaded?"; the package trades on the second framing.
//
// **Invocation pattern:** `refresolve.Detect(ctx, text)
// ([]Reference, error)` — pure-Go regex + list-match for nine
// deterministic shapes; Qwen rubric (cold-start) via
// `go/internal/measure/` for domain-term shape; capitalized
// multi-word heuristic for external-technical shape. T3 adds
// `refresolve.Dispatch(ctx, refs) (map[Reference]HitSet, error)`
// alongside; T7 swaps the Qwen rubric for a trained classifier
// when the ML capability chain ships.
//
// **Success shape:** one `Reference` per detected token with
// `{Token, Shape, Confidence, DetectionMethod, StartPos, EndPos}`.
// Total Detect call caps at 600ms; rule-based detectors return in
// <5ms for messages up to 1000 chars. The same input produces the
// same output across calls (deterministic for rule-based;
// fixed-seed where available for the rubric path).
//
// **Non-goals:** does not resolve references to bindings (T3's
// resolver registry does that); does not auto-fire on every
// message (the v1 reference-resolution skill governs when the
// agent calls in; auto-firing is proactive-injection territory
// and stays separate); does not enforce confidence-tier rules
// (the dispatcher in T3 classifies tiers per the design doc's
// single source of truth at docs/REFERENCE_RESOLUTION.md §3.4).
package refresolve
