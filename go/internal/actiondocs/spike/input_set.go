package spike

// SpikeQuery is one entry in the spike's evaluation set. Question is what
// an agent would actually type. ExpectedHit names the (surface, action)
// chunk that SHOULD land in the top-3 — the gold target for hit-quality
// measurement. Rationale captures why this query is representative; it
// gets surfaced in the decision artifact so the input-set choices are
// auditable.
type SpikeQuery struct {
	Question    string
	ExpectedHit struct {
		Surface string
		Action  string
	}
	Rationale string
}

// InputSet is the curated evaluation corpus for the action-docs spike.
// Seven queries shaped by the friction patterns named in the chain's
// design_decisions: alias lookup, error-condition lookup, comparison,
// surface convention lookup, sentinel-value lookup, return-shape, and
// alternative-action discovery. Sized to keep the spike time-boxed
// (running every prototype against this set should take seconds, not
// minutes).
var InputSet = []SpikeQuery{
	{
		Question: "Does bug_resolve accept sha as an alias for commit_sha?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "bug_resolve"},
		Rationale: "Alias-lookup friction — the exact failure mode the workDescription wall-of-prose forced agents to scan for. If keyword-match returns bug_resolve in top-3 for this, Shape B is plausibly useful.",
	},
	{
		Question: "What's the error when forge is called without schema_name?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "forge"},
		Rationale: "Error-condition lookup. The schema_name top-level requirement is named in workDescription's IMPORTANT paragraph; the chunk for forge captures it as an [[errors]] entry. Tests whether the prototype can route a 'what's the error when X' question.",
	},
	{
		Question: "How does roadmap_update differ from roadmap_set?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "roadmap_update"},
		Rationale: "Comparison question. Both chunks reference each other in their notes (full-replace vs partial-update); we want the more-specific one to surface (roadmap_update for 'differ from' phrasing), not roadmap_set (the broader topic).",
	},
	{
		Question: "Can I pass project on a read action?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "_general"},
		Rationale: "Cross-cutting convention question. The answer lives in work/_general (cross-project default). Tests whether _general gets surfaced for convention-shaped questions or whether keyword match drowns it in per-action hits.",
	},
	{
		Question: "What does commit_sha=unversioned mean?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "_general"},
		Rationale: "Sentinel-value lookup. The 'unversioned' sentinel is documented in work/_general as a cross-cutting note covering all four SHA-accepting actions. Could plausibly hit bug_resolve / bug_stamp_sha / task_complete / task_stamp_sha instead — those also reference 'unversioned' in their notes. _general winning would suggest the cross-cutting prose pattern works; one of the per-action chunks winning would suggest the cross-cutting pattern is brittle.",
	},
	{
		Question: "How do I close a chain - is forge_delete OK for chain?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"work", "chain_close"},
		Rationale: "Alternative-discovery question. forge_delete and chain_close are both relevant; we want the chunk that explains 'use chain_close, forge_delete is rejected' (chain_close) to land first. Tests whether the prototype handles 'is X OK for Y' framing.",
	},
	{
		Question: "Which classify actions return label, latency_ms, and model_name?",
		ExpectedHit: struct {
			Surface string
			Action  string
		}{"measure", "_general"},
		Rationale: "Surface-wide question about shared response shape. The {label, latency_ms, model_name} triple is documented in measure/_general; every classify chunk also includes it. _general winning suggests cross-cutting prose is discoverable; otherwise the prototype routes to whichever classify_* chunk has the closest keyword overlap.",
	},
}
