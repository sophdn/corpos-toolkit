package refresolve

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/events"
	"toolkit/internal/knowledge"
	"toolkit/internal/obs"
	"toolkit/internal/refresolve/refparams"
)

// ResolveReferencesResult is the typed response shape for both the
// resolve_references and parse_context actions on the knowledge
// meta-tool. resolve_references is an alias of parse_context
// (reference-resolution-migration T5); the two actions share this
// envelope and the new fields (CacheHits, CacheMisses) stay at
// zero in the resolve_references flow until the cache layer lands.
//
// Telemetry field semantics (bug 1428):
//   - ResolutionTimeMs is the handler's full wall-clock: LoadCatalogs +
//     Detect + cache lookup + Dispatch + result formatting + grounding
//     events emit. NOT the cumulative resolver work — that's
//     ResolverWorkMs.
//   - ResolverCallsMade counts refs that went through Dispatch (i.e.
//     refs not served from cache). Equal to CacheMisses when the cache
//     is active; otherwise equal to len(References) + len(NoHitTokens).
//   - ResolverWorkMs sums HitSet.RetrievalCostMs across all freshly-
//     resolved refs. A resolver that returns empty candidates in
//     sub-1ms still counts as a CacheMiss / ResolverCall but adds zero
//     to ResolverWorkMs. Distinguishes "many misses, no work" from
//     "many misses, lots of work."
//   - CacheHits / CacheMisses count refs (not tokens). A token detected
//     under two shapes adds two to CacheHits+CacheMisses combined.
type ResolveReferencesResult struct {
	References        []ResolvedReference `json:"references"`
	ResolutionTimeMs  int64               `json:"resolution_time_ms"`
	ResolverCallsMade int                 `json:"resolver_calls_made"`
	ResolverWorkMs    int64               `json:"resolver_work_ms,omitempty"`
	NoHitTokens       []string            `json:"no_hit_tokens,omitempty"`
	PartialFailures   []string            `json:"partial_failures,omitempty"`
	TruncatedByBudget bool                `json:"truncated_by_budget,omitempty"`
	// CacheHits / CacheMisses are populated by parse_context's filter
	// layer (reference-resolution-migration T5). resolve_references
	// callers see zero values until the layer is wired.
	CacheHits   int    `json:"cache_hits,omitempty"`
	CacheMisses int    `json:"cache_misses,omitempty"`
	Error       string `json:"error,omitempty"`
	// InlinedBytes + InlinedRefs surface skill-body inlining totals
	// (chain 602). Zero unless the inline-body feature flag is on AND
	// the envelope carries at least one use_directly skill_trigger or
	// discipline_skill ref. See body_inliner.go.
	InlinedBytes int `json:"inlined_bytes,omitempty"`
	InlinedRefs  int `json:"inlined_refs,omitempty"`
	// Intent is the resolved directive-intent shape from DetectIntent
	// (chain parse-context-lean-orienting T5; design at docs/PARSE_CONTEXT.md
	// §13). Pointer-on-omitempty: absent when intent detection didn't
	// fire (degraded boot / disabled). When present, downstream
	// resolvers MAY condition their behavior on Intent.Shape; the
	// invariant from §13.3 is that missing intent NEVER gates the
	// existing token-shape resolvers.
	Intent *IntentResultEnvelope `json:"intent,omitempty"`
	// CandidateDisciplines carries the raw intent-mapped disciplines that APPLY
	// to the message (mapping + entryApplies + opt-out + manifest), WITHOUT the
	// firing policy (per-envelope cap, dedup, recent-fire suppression). Populated
	// ONLY when the caller passes discipline_firing="client" (corpos, chain
	// toolkit-decomposition T5); in that mode no budgeted disciplines are added to
	// References, so the client owns the firing cadence. Empty in the default
	// server-firing mode.
	CandidateDisciplines []ResolvedReference `json:"candidate_disciplines,omitempty"`
}

// ResolvedReference is one detected-and-resolved reference, ready
// for the agent to incorporate into its response per the v1 skill's
// "named-what-you-found" discipline.
type ResolvedReference struct {
	Token             string                     `json:"token"`
	Shape             ShapeCategory              `json:"shape"`
	ConfidenceTier    ConfidenceTier             `json:"confidence_tier"`
	PresentedAs       string                     `json:"presented_as"`
	TopCandidates     []Candidate                `json:"top_candidates"`
	RecommendedAction PresentationRecommendation `json:"recommended_action"`
	GroundingEventID  int64                      `json:"grounding_event_id,omitempty"`
	// FromCache / CachePolicy are populated by parse_context's filter
	// layer. Zero / empty until the cache wires (T5 follow-on).
	FromCache   bool   `json:"from_cache,omitempty"`
	CachePolicy string `json:"cache_policy,omitempty"`
	// Skill-body inlining (chain 602). All four fields are populated
	// by the body inliner that runs between Dispatch and output
	// formatting; only refs with Shape∈{SkillTrigger,DisciplineSkill}
	// AND ConfidenceTier=TierSingleExact are eligible. When the
	// feature flag is off, all four stay zero/empty.
	BodyInlined   string `json:"body_inlined,omitempty"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	BodySummary   string `json:"body_summary,omitempty"`
	BodyBytes     int    `json:"body_bytes,omitempty"`
	// When non-nil: this ref's body was deduped because an earlier ref
	// in the same response already carries the inlined body for the
	// same skill. The integer is the 0-based index into the response's
	// References slice where BodyInlined / BodySummary live. BodyBytes
	// is still set on the deduped ref (the agent knows the body's
	// size); BodyInlined / BodySummary stay empty so the envelope
	// doesn't carry the same body twice. Added 2026-05-21 as a
	// follow-on to chain parse-context-skill-body-inline-on-use-directly
	// to eliminate the multi-trigger-same-skill duplication observed
	// during T6 smoke (2 trigger keywords → 2× full body inlined).
	BodyInlinedFromRefIndex *int `json:"body_inlined_from_ref_index,omitempty"`
}

// IntentResultEnvelope is the wire shape of the parse_context
// envelope's top-level `intent` field (chain parse-context-lean-
// orienting T5; design pinned in docs/PARSE_CONTEXT.md §13.4).
// Pointer-on-omitempty so callers that don't care about intent see
// the same envelope shape as before; populated by every call that
// ran the intent detector.
type IntentResultEnvelope struct {
	Shape       string  `json:"shape"`
	Confidence  float64 `json:"confidence"`
	DetectedVia string  `json:"detected_via"`
}

// PresentationRecommendation tells the agent what to do with the
// resolved reference per design doc §4.1.
type PresentationRecommendation string

const (
	PresentUseDirectly               PresentationRecommendation = "use_directly"
	PresentAskUserToDisambiguate     PresentationRecommendation = "ask_user_to_disambiguate"
	PresentMentionAsPossiblyRelevant PresentationRecommendation = "mention_as_possibly_relevant"
	PresentAcknowledgeNoHitAndAsk    PresentationRecommendation = "acknowledge_no_hit_and_ask"
)

// HandlerDeps bundles the dependencies the resolve_references
// handler needs. Constructed once at server startup; the handler
// reloads catalogs per call so newly-forged chains/tasks/bugs are
// detectable without server restart.
type HandlerDeps struct {
	// Pool is the toolkit-server DB pool. Required.
	Pool *db.Pool
	// Project is the project scope for library lookups and
	// telemetry rows.
	Project string
	// KnowledgeDeps carries the knowledge meta-tool's dependencies
	// (router for Qwen rerank, vault root, kiwix base URL).
	// Required.
	KnowledgeDeps knowledge.Deps
	// RepoRoot points at the toolkit-server checkout for filesystem
	// catalogs (skill/tool/schema lookups and path resolver
	// fallback). Required for filesystem-shape detection.
	RepoRoot string
	// Classifier is the domain-term classifier wrapper. May be nil
	// for tests / degraded mode; the detector skips domain-term
	// detection rather than failing.
	Classifier DomainTermClassifier
	// Registry is the resolver registry, built once at startup via
	// BuildProductionRegistry. The handler reuses the DB- and
	// knowledge-backed resolvers across calls, but per call it overlays
	// fresh skill_trigger / discipline_skill / skill_candidate resolvers
	// rebuilt from the reloaded manifest (Registry.CloneWith) so skills
	// added to the manifest after boot resolve without a restart (bug 884).
	Registry *Registry
	// Cache is the parse_context filter layer. May be nil — when nil,
	// the handler skips cache lookup and population (each call resolves
	// from scratch). Constructed once at startup via
	// NewParseContextCache and shared across handler calls.
	Cache *ParseContextCache
	// MemoryDir points at the user's auto-memory directory for this
	// project (typically ~/.claude/projects/<cwd-slug>/memory/). When
	// empty, the memory_entry resolver runs in shell mode (TierNoHit
	// always). Reference-resolution-migration T10.
	MemoryDir string
	// BodyCache memoises skill body reads (path + mtime keyed). Shared
	// across handler calls so the same body is read once per session,
	// not once per envelope. May be nil; the inliner constructs a
	// per-call cache as a fallback. Chain 602.
	BodyCache *BodyCache
	// GitSHA is the running binary's compile-time gitSHA. Used by the
	// stdio-drift surface to label the on-disk binary in the snapshot
	// (chain parse-context-lean-orienting T9). Empty when the binary
	// was built without ldflags injection (degraded boot, smoke,
	// `go run`); the drift surface still works on the fd_deleted
	// signal but reported_git_sha may stay empty.
	GitSHA string
	// DriftFireTracker is the per-MCP-session counter the stdio-drift
	// surface consults when deciding whether to fire a Candidate.
	// Nil-safe — when absent, the substrate emits the
	// ParseContextStdioDriftSurfaced event for telemetry but doesn't
	// surface a Candidate.
	DriftFireTracker *DriftFireTracker
	// DriftMarkerPathOverride and DriftProcRootOverride exist for
	// tests that need to inject controlled marker / /proc fixtures.
	// Empty strings in production preserve the canonical paths
	// (stdiodrift.MarkerPath and "/proc"). Not surfaced through any
	// MCP / HTTP param.
	DriftMarkerPathOverride string
	DriftProcRootOverride   string
	// WorkStateCache holds resolved work-state surfacings per
	// (sessionID, project, intent) with the short-5-turns TTL (chain
	// parse-context-lean-orienting T6). Nil-safe — when absent the
	// work-state resolver runs on every call (no caching). The
	// cache-invalidation fold hook in cache_invalidate.go drops every
	// entry on chain/task/bug emits so subsequent calls see fresh
	// state.
	WorkStateCache *WorkStateCache
	// DisciplineFireTracker is the per-MCP-session counter the intent
	// → discipline surface consults for the recent-fire suppression
	// rule (chain parse-context-lean-orienting T7). Nil-safe — when
	// absent every fire is treated as a first-fire.
	DisciplineFireTracker *DisciplineFireTracker
	// KiwixFallbackSearch is the injectable kiwix search the low-
	// confidence fallback surface (chain parse-context-lean-orienting
	// T8) calls when the gate arms. Nil-safe — when absent the gate
	// short-circuits without emitting (no evaluation telemetry, distinct
	// from a recorded suppress). Production wiring uses
	// kiwixFallbackSearcherFromKnowledge(KnowledgeDeps, Project); tests
	// inject a stub.
	KiwixFallbackSearch KiwixFallbackSearchFn
}

// parseContextParams is the typed request shape for both the parse_context
// action (canonical) and resolve_references (alias). It now lives in the
// dependency-light leaf package refparams so the knowledge action-doc registry
// can reflect it for type derivation without package knowledge importing
// refresolve (which would cycle — refresolve imports knowledge). The local alias
// keeps every handler reference (resolveSessionID, resolveInlineOpts, the
// json.Unmarshal target) unchanged. See refparams/refparams.go and chain
// migrate-knowledge-action-docs-to-derive-contract.
type parseContextParams = refparams.ParseContextParams

// HandleResolveReferences is the typed handler entrypoint for the
// resolve_references alias. parse_context is the canonical name; the
// alias dispatches into the same core so existing callers keep
// working without modification (reference-resolution-migration T5).
func HandleResolveReferences(ctx context.Context, deps HandlerDeps, params json.RawMessage) (ResolveReferencesResult, error) {
	return handleParseContextCore(ctx, deps, params, "resolve_references")
}

// HandleParseContext is the typed handler entrypoint for the
// canonical parse_context action. Same core as resolve_references
// today; the cache + new-resolver coverage land in follow-on phases
// of T5.
func HandleParseContext(ctx context.Context, deps HandlerDeps, params json.RawMessage) (ResolveReferencesResult, error) {
	return handleParseContextCore(ctx, deps, params, "parse_context")
}

func handleParseContextCore(ctx context.Context, deps HandlerDeps, params json.RawMessage, actionName string) (ResolveReferencesResult, error) {
	if deps.Registry == nil {
		return ResolveReferencesResult{Error: "registry not configured"}, nil
	}
	var p parseContextParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ResolveReferencesResult{Error: fmt.Sprintf("parse params: %s", err)}, nil
		}
	}
	if p.MessageText == "" {
		return ResolveReferencesResult{
			References: []ResolvedReference{},
		}, nil
	}
	if p.TopKPerShape <= 0 {
		p.TopKPerShape = 5
	}

	start := time.Now()

	// Reload catalogs each call so newly-forged chains/tasks/bugs
	// are detectable without restart. The DB queries are cheap
	// (~5ms total) and amortize against the detector's rule-based
	// path budget.
	cats, err := LoadCatalogs(ctx, deps.RepoRoot, deps.Pool, deps.MemoryDir)
	if err != nil {
		return ResolveReferencesResult{Error: fmt.Sprintf("load catalogs: %s", err)}, nil
	}

	// Overlay manifest-backed resolvers per call so a skill added to the
	// manifest after boot resolves without a daemon restart (bug 884). See
	// prepareRegistry for the rationale.
	registry := prepareRegistry(deps, cats)

	detector := NewDetector(cats, deps.Classifier)
	refs, err := detector.Detect(ctx, p.MessageText)
	if err != nil {
		return ResolveReferencesResult{Error: fmt.Sprintf("detect: %s", err)}, nil
	}

	sessionID := resolveSessionID(ctx, p)
	bypassCache := p.CachePolicyOverride == "fresh"

	// Cache split: each detected ref either pulls cached candidates or queues
	// for fresh resolution (per-shape admissibility lives in cache.go).
	cachedHits, cachedPolicies, refsToResolve := splitCachedRefs(deps, sessionID, bypassCache, refs)

	dispatchOpts := DispatchOptions{
		MaxCandidatesPerResolver: p.TopKPerShape,
	}
	if p.TotalBudgetMs > 0 {
		dispatchOpts.TotalBudget = time.Duration(p.TotalBudgetMs) * time.Millisecond
	}
	// Apply defaults locally so the TruncatedByBudget check below sees
	// the same TotalBudget the dispatcher will actually enforce — when
	// the caller omits total_budget_ms, the dispatcher silently uses 2s
	// but the handler's local copy would otherwise stay at zero and
	// the truncation flag would never fire.
	dispatchOpts = dispatchOpts.applyDefaults()
	freshHits, err := Dispatch(ctx, registry, refsToResolve, dispatchOpts)
	if err != nil {
		return ResolveReferencesResult{Error: fmt.Sprintf("dispatch: %s", err)}, nil
	}

	// Populate cache with fresh-resolved hits whose shape policy
	// admits caching. TierNoHit hits don't get cached — a future
	// catalog change may turn them into real bindings; caching the
	// negative would hide it.
	if deps.Cache != nil && !bypassCache {
		for ref, hs := range freshHits {
			if hs.Err == nil && hs.ConfidenceTier != TierNoHit {
				deps.Cache.Put(sessionID, ref.Token, ref.Shape, hs)
			}
		}
	}

	// Merge cached + fresh into a single map for telemetry + output
	// formatting. Cached entries take precedence (defensive — should
	// never both be present for the same ref since refsToResolve
	// excludes cache hits).
	hits := make(map[Reference]HitSet, len(refs))
	for ref, hs := range freshHits {
		hits[ref] = hs
	}
	for ref, hs := range cachedHits {
		hits[ref] = hs
	}

	// Emit one grounding_events row per detected reference per design
	// doc §6.2 (Path A: query_source='reference_resolution' lands as a
	// first-class enum value after migration 040). Each row carries a
	// synthesized call_id ("<span>#r<i>") so the (session_id, call_id)
	// unique constraint admits N rows from one tools/call. Failure to
	// emit is logged and dropped — telemetry must not block resolution.
	// Cache-hit refs still emit so the agent's query stream stays
	// visible in grounding_events even when no fresh resolver work
	// happened.
	groundingIDs := emitGroundingEvents(ctx, deps, actionName, p.MessageText, refs, hits)

	// Bug 1428: surface cumulative resolver wall-clock so a 10ms-handler
	// + 10-cache-miss envelope is legible as "10 misses, 8ms of work
	// done" rather than confusing the agent into suspecting a timer
	// bug. Sums RetrievalCostMs across freshly-dispatched refs only;
	// cache hits add zero.
	var resolverWorkMs int64
	for _, hs := range freshHits {
		resolverWorkMs += hs.RetrievalCostMs
	}

	out := ResolveReferencesResult{
		ResolverCallsMade: len(freshHits),
		ResolverWorkMs:    resolverWorkMs,
	}
	out.References, out.NoHitTokens, out.PartialFailures, out.CacheHits, out.CacheMisses =
		assembleReferences(refs, hits, cachedHits, cachedPolicies, groundingIDs, p.IncludeNoHits, deps.Cache != nil)

	// Skill-body inlining (chain 602). Runs after references are
	// assembled so the inliner can mutate the slice in place. No-op
	// when the feature flag is off; when on, populates BodyInlined /
	// BodyTruncated / BodySummary / BodyBytes on eligible refs and
	// reports envelope-level totals. The skill manifest from cats
	// powers the bucket-based precedence rule when the envelope
	// budget is pressed.
	inlineOpts := resolveInlineOpts(p, deps, cats.SkillManifest)
	inlinedBytes, inlinedRefs := applyBodyInlining(out.References, inlineOpts)
	out.InlinedBytes = inlinedBytes
	out.InlinedRefs = inlinedRefs

	// Directive-intent detection (chain parse-context-lean-orienting
	// T5; design at docs/PARSE_CONTEXT.md §13). Pure-regex,
	// sub-100μs typical; threaded into both the envelope's top-level
	// `intent` field AND the T9 drift surface so verify/fix/implement/
	// audit prompts unlock the intent-conditional drift Candidate
	// path. The invariant from §13.3: missing intent NEVER gates the
	// existing token-shape resolvers — even with IntentNone the rest
	// of the envelope resolves normally.
	intentStart := time.Now()
	intent := DetectIntent(p.MessageText)
	intentLatencyMs := int(time.Since(intentStart).Milliseconds())
	out.Intent = &IntentResultEnvelope{
		Shape:       string(intent.Shape),
		Confidence:  intent.Confidence,
		DetectedVia: intent.DetectedVia,
	}

	// Post-resolution surfacing passes, in FIXED order: intent-mapped
	// disciplines → work-state → stdio-drift → kiwix fallback. The order is
	// load-bearing — kiwix runs last so its gate inspects the fully-assembled
	// References (everything the earlier passes appended). Each pass owns its
	// own firing / dedup / emit logic and is deliberately NOT unified behind a
	// common abstraction: they answer to different reasons-to-change (see the
	// chain's audit ledger, F1/Q6).
	// Discipline firing: server mode (default) budgets + surfaces inline (Claude
	// Code); client mode returns the raw applicable disciplines for a client that
	// owns the firing policy (corpos) and surfaces none inline (T5).
	if p.DisciplineFiring == "client" {
		out.CandidateDisciplines = RawIntentDisciplines(cats.SkillManifest, intent.Shape, p.MessageText)
	} else {
		surfaceIntentDisciplines(ctx, deps, &out, cats.SkillManifest, intent.Shape, p.MessageText, sessionID)
	}
	surfaceWorkState(ctx, deps, &out, intent.Shape, sessionID)
	surfaceDrift(ctx, deps, &out, intent.Shape, sessionID)
	surfaceKiwixFallback(ctx, deps, &out, refs, hits, intent.Shape, p.MessageText, sessionID)

	// Intent-resolution event for T10's measurement (chain
	// parse-context-lean-orienting T5 acceptance criteria). Fires on
	// every parse_context call that ran the intent detector; the
	// drift event above and this one share the same async-emit
	// pathway so neither blocks the request goroutine.
	emitIntentResolvedEvent(ctx, deps, sessionID, p.MessageText, intent, intentLatencyMs)

	out.ResolutionTimeMs = time.Since(start).Milliseconds()
	if dispatchOpts.TotalBudget > 0 && out.ResolutionTimeMs >= dispatchOpts.TotalBudget.Milliseconds() {
		out.TruncatedByBudget = true
	}
	return out, nil
}

// prepareRegistry overlays the three manifest-backed resolvers (skill_trigger,
// discipline_skill, skill_candidate) onto deps.Registry from the freshly
// reloaded manifest, so a skill added after boot RESOLVES (not just DETECTS)
// without a daemon restart (bug 884). CloneWith leaves the shared registry
// untouched (concurrent-call safe) — three struct allocations, no extra I/O
// (the manifest is already in cats). Absent manifest (RepoRoot empty / no
// _manifest.toml) → the boot-time registry unchanged.
func prepareRegistry(deps HandlerDeps, cats Catalogs) *Registry {
	if cats.SkillManifest == nil {
		return deps.Registry
	}
	return deps.Registry.CloneWith(
		NewSkillTriggerResolver(cats.SkillManifest),
		NewDisciplineSkillResolver(cats.SkillManifest),
		NewSkillCandidateResolver(cats.SkillManifest),
	)
}

// resolveSessionID picks the cache-scope key by precedence: explicit param >
// stable MCP-session id (stamped per-connection by the transport wrapper) >
// current span's trace-id > empty (cache disabled for this call). The
// MCP-session id is load-bearing in production: agent callers don't thread
// session_id, and span.TraceID is freshly minted per tools/call — so falling
// straight to it produced a different key per call and the cache never hit
// (chain parse-context-lean-orienting T1).
func resolveSessionID(ctx context.Context, p parseContextParams) string {
	if p.SessionID != "" {
		return p.SessionID
	}
	if id := events.MCPSessionIDFromContext(ctx); id != "" {
		return id
	}
	if s := obs.SpanFromContext(ctx); s != nil {
		return s.TraceID
	}
	return ""
}

// splitCachedRefs partitions detected refs into cache hits (with their stored
// policy) and the refs that still need fresh resolution. With a nil cache or a
// 'fresh' bypass, every ref queues for resolution.
func splitCachedRefs(deps HandlerDeps, sessionID string, bypassCache bool, refs []Reference) (cachedHits map[Reference]HitSet, cachedPolicies map[Reference]CachePolicy, refsToResolve []Reference) {
	cachedHits = make(map[Reference]HitSet)
	cachedPolicies = make(map[Reference]CachePolicy)
	refsToResolve = make([]Reference, 0, len(refs))
	for _, ref := range refs {
		if deps.Cache != nil && !bypassCache {
			if hs, policy, ok := deps.Cache.Get(sessionID, ref.Token, ref.Shape); ok {
				cachedHits[ref] = hs
				cachedPolicies[ref] = policy
				continue
			}
		}
		refsToResolve = append(refsToResolve, ref)
	}
	return cachedHits, cachedPolicies, refsToResolve
}

// assembleReferences builds the resolved-reference slice, the no-hit-token
// bucket, the partial-failure list, and the cache hit/miss counts from the
// detected refs and their resolved HitSets. Sans-IO: a pure function of its
// inputs (PolicyForShape is a pure shape→policy lookup), so it is unit-testable
// in isolation. Preserves the envelope invariants: per-token no-hit dedup, and
// a no-hit token that also resolved under another shape is dropped from
// NoHitTokens so intersect(refs_tokens, no_hit_tokens) stays empty (bug 1426).
// References is always non-nil (so the wire carries [] not null).
func assembleReferences(refs []Reference, hits, cachedHits map[Reference]HitSet, cachedPolicies map[Reference]CachePolicy, groundingIDs []int64, includeNoHits, cacheActive bool) (references []ResolvedReference, noHitTokens, partialFailures []string, cacheHits, cacheMisses int) {
	references = make([]ResolvedReference, 0, len(refs))
	resolvedTokens := make(map[string]struct{}, len(refs))
	noHitTokensSeen := make(map[string]struct{}, len(refs))
	pendingNoHits := make([]string, 0, len(refs))
	for i, ref := range refs {
		hs := hits[ref]
		_, fromCache := cachedHits[ref]
		if cacheActive {
			if fromCache {
				cacheHits++
			} else {
				cacheMisses++
			}
		}
		if hs.ConfidenceTier == TierNoHit && !includeNoHits {
			if hs.Err != nil {
				partialFailures = append(partialFailures,
					fmt.Sprintf("%s/%s: %s", ref.Shape, ref.Token, hs.Err.Error()))
			} else if _, dup := noHitTokensSeen[ref.Token]; !dup {
				noHitTokensSeen[ref.Token] = struct{}{}
				pendingNoHits = append(pendingNoHits, ref.Token)
			}
			continue
		}
		resolved := formatResolved(ref, hs)
		if i < len(groundingIDs) {
			resolved.GroundingEventID = groundingIDs[i]
		}
		if cacheActive {
			if fromCache {
				resolved.FromCache = true
				resolved.CachePolicy = string(cachedPolicies[ref])
			} else {
				// Document the policy that will apply on a future call.
				resolved.CachePolicy = string(PolicyForShape(ref.Shape))
			}
		}
		resolvedTokens[ref.Token] = struct{}{}
		references = append(references, resolved)
	}
	// Drop any no-hit token that also has a resolved reference. Keeps the
	// envelope invariant: intersect(refs_tokens, no_hit_tokens) is empty — the
	// agent gets exactly one verdict per token.
	for _, tok := range pendingNoHits {
		if _, hit := resolvedTokens[tok]; hit {
			continue
		}
		noHitTokens = append(noHitTokens, tok)
	}
	return references, noHitTokens, partialFailures, cacheHits, cacheMisses
}

// surfaceIntentDisciplines appends intent-mapped discipline Candidates (chain
// parse-context-lean-orienting T7). It dedups against disciplines already in
// out.References (keyword-triggered evidence wins) and is cap/recent-fire
// budgeted inside ResolveIntentDisciplines. Always emits its telemetry event.
func surfaceIntentDisciplines(ctx context.Context, deps HandlerDeps, out *ResolveReferencesResult, manifest *SkillManifest, intent IntentShape, messageText, sessionID string) {
	alreadySurfaced := map[string]bool{}
	for _, ref := range out.References {
		if ref.Shape == ShapeDisciplineSkill {
			alreadySurfaced[ref.Token] = true
		}
	}
	refs, tel := ResolveIntentDisciplines(ctx, manifest, intent, messageText, sessionID, alreadySurfaced, deps.DisciplineFireTracker)
	out.References = append(out.References, refs...)
	emitDisciplineSurfacedEvent(ctx, deps, sessionID, tel)
}

// surfaceWorkState appends open-work Candidates (open bugs / active tasks /
// recent chains) on work-shape intents (chain parse-context-lean-orienting
// T6); no-op on docs intents and IntentNone. A query error drops the refs but
// the telemetry event still emits.
func surfaceWorkState(ctx context.Context, deps HandlerDeps, out *ResolveReferencesResult, intent IntentShape, sessionID string) {
	refs, tel, err := ResolveWorkState(ctx, deps.Pool, deps.Project, intent, deps.WorkStateCache, sessionID)
	if err == nil {
		out.References = append(out.References, dedupWorkStateRefs(out.References, refs)...)
	}
	emitWorkStateEvent(ctx, deps, sessionID, intent, tel)
}

// dedupWorkStateRefs drops work-state surfacings whose token already
// appears among the token-resolved references — a named chain/task/bug
// that slug detection already surfaced must not be double-listed by the
// work-state pass (chain parse-context-directive-intent-extension §14.4;
// the execute-intent named-slug case is the motivating one, but the dedup
// is general — it also de-dupes the long-standing status/verify "name a
// chain that is also recent work-state" overlap). Token-equality is the
// key: work-state refs are chain/task/bug slugs, the same string the slug
// resolvers emit. The telemetry counts (tel) stay raw — they report what
// the work-state queries found; this is a presentation-layer dedup.
func dedupWorkStateRefs(existing, workState []ResolvedReference) []ResolvedReference {
	if len(existing) == 0 {
		return workState
	}
	seen := make(map[string]bool, len(existing))
	for _, r := range existing {
		if r.Token != "" {
			seen[r.Token] = true
		}
	}
	out := make([]ResolvedReference, 0, len(workState))
	for _, r := range workState {
		if seen[r.Token] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// surfaceDrift conditionally appends a stdio-binary-drift Candidate (chain
// parse-context-lean-orienting T9). The event emits on every call (fire AND
// suppress outcomes feed T10's drift-fire-rate measurement) regardless of
// whether a Candidate surfaced.
func surfaceDrift(ctx context.Context, deps HandlerDeps, out *ResolveReferencesResult, intent IntentShape, sessionID string) {
	driftState := snapshotDrift(ctx, deps)
	surface, bootstrap, suppressed := deps.DriftFireTracker.shouldSurface(sessionID, driftState, string(intent))
	if surface {
		out.References = append(out.References, driftCandidate(driftState))
	}
	emitDriftEvent(ctx, deps, sessionID, driftState, bootstrap, suppressed)
}

// surfaceKiwixFallback is the LAST surfacing pass (chain parse-context-lean-
// orienting T8): it inspects the fully-assembled out.References so its
// single_exact gate observes everything the earlier passes landed. The
// no-hit-refs slice is rebuilt from the original refs + hits map because the
// envelope's NoHitTokens is dedup'd-by-token and strips the shape the gate
// needs (external_technical OR domain_term).
func surfaceKiwixFallback(ctx context.Context, deps HandlerDeps, out *ResolveReferencesResult, refs []Reference, hits map[Reference]HitSet, intent IntentShape, messageText, sessionID string) {
	noHitRefs := make([]Reference, 0, len(refs))
	for _, ref := range refs {
		if hs, ok := hits[ref]; ok && hs.ConfidenceTier == TierNoHit {
			noHitRefs = append(noHitRefs, ref)
		}
	}
	kiwixRefs, tel := ResolveKiwixFallback(ctx, out.References, noHitRefs, intent, messageText, deps.KiwixFallbackSearch)
	out.References = append(out.References, kiwixRefs...)
	emitKiwixFallbackEvent(ctx, deps, sessionID, tel)
}

// BuildResolveReferencesHandler wraps HandleResolveReferences for
// the dispatch table. The server startup calls this once and
// inserts the resulting dispatch.Handler under "resolve_references"
// on the knowledge meta-tool's dispatch.Table.
func BuildResolveReferencesHandler(deps HandlerDeps) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ResolveReferencesResult, error) {
		return HandleResolveReferences(ctx, depsWithProject(deps, project), params)
	})
}

// BuildParseContextHandler wraps HandleParseContext for the dispatch
// table. The server startup registers this under "parse_context" on
// the knowledge meta-tool; resolve_references stays registered as a
// soft alias (reference-resolution-migration T5).
func BuildParseContextHandler(deps HandlerDeps) dispatch.Handler {
	return dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (ResolveReferencesResult, error) {
		return HandleParseContext(ctx, depsWithProject(deps, project), params)
	})
}

// depsWithProject returns a copy of deps with the per-call project
// substituted in when non-empty. Bug 1451: the dispatcher's project
// resolver picks a project per call (from args.Project or cwd-match),
// but the handler factory was throwing that string away and using the
// static deps.Project captured at server startup — which is empty for
// stdio sessions that boot without --default-project. Empty Project
// flowed through to grounding_events.project_id, breaking the
// (project_id, source_ref) JOIN against knowledge_pointers and zeroing
// out the Context Pull Inspector's first_candidate.source_type column.
//
// When the per-call project is empty (no Project arg, no cwd match, no
// default), we fall back to deps.Project — preserves test ergonomics
// (HandleResolveReferences called directly with a hand-built deps) and
// avoids inventing a project the caller didn't authorise.
func depsWithProject(deps HandlerDeps, project string) HandlerDeps {
	if project == "" {
		return deps
	}
	deps.Project = project
	return deps
}
