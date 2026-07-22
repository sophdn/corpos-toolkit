// Package knowledge hosts MCP action handlers for the knowledge meta-tool:
// vault_search, vault_read, kiwix_search, kiwix_fetch, library_*, reference_*,
// knowledge_search, knowledge_fetch, knowledge_report_miss.
//
// Ported per PARITY_STANDARD.md from the Rust toolkit-server's
// dispatch/knowledge.rs.
package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"toolkit/internal/db"
	"toolkit/internal/inference/router"
	"toolkit/internal/jsonutil"
	"toolkit/internal/knowledge/kiwix"
	"toolkit/internal/knowledge/vault"
	"toolkit/internal/mcpparam"
	"toolkit/internal/qwenctx"
	"toolkit/internal/qwenretrieve"
	"toolkit/internal/stringutil"
)

// Deps holds shared dependencies for knowledge handlers.
type Deps struct {
	Pool   *db.Pool
	Router *router.Router
	// VaultRoot overrides the default vault path ($HOME/.claude/vault). Empty
	// string uses the resolved default; tests inject a temp dir.
	VaultRoot string
	// KiwixBaseURL overrides the default kiwix-serve endpoint. Empty string
	// falls back to $KIWIX_BASE_URL then kiwix.DefaultBaseURL.
	KiwixBaseURL string
	// KiwixClient is a test-only override; production code leaves it nil so
	// buildKiwixClient constructs a fresh client per call.
	KiwixClient *kiwix.Client
}

// MaxVaultCandidates caps the pass-1 candidate list before Qwen rerank.
// Sized for the Qwen 8192-token context: 8192 − 620 overhead ≈ 7572 budget;
// observed ~100 tokens per entry → 75 with headroom. Matches the Rust cap.
// Overridable at runtime via the TOOLKIT_VAULT_MAX_CANDIDATES env var
// (per bug 655 — agents who hit candidates_truncated need a documented
// knob to bump). Read via effectiveMaxCandidates().
const MaxVaultCandidates = 75

// MaxVaultCandidatesEnvVar is the runtime override for MaxVaultCandidates.
// Set to a positive integer to raise/lower the cap without rebuilding.
// Invalid or unset values fall back to MaxVaultCandidates.
const MaxVaultCandidatesEnvVar = "TOOLKIT_VAULT_MAX_CANDIDATES"

// effectiveMaxCandidates returns the runtime-effective vault-candidate cap:
// the env-var override if set to a positive integer, else MaxVaultCandidates.
func effectiveMaxCandidates() int {
	if v := os.Getenv(MaxVaultCandidatesEnvVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return MaxVaultCandidates
}

// Pass2BodyExcerptChars is the per-candidate body-excerpt cap for pass-2.
// Sized so a 5-candidate pass-2 prompt fits in ~700 input tokens. Mirrors Rust.
const Pass2BodyExcerptChars = 700

// VaultSearchRetryAttempts is the cold-start retry budget for the inference
// dispatch (treats EmptyResponse as transient). Matches the Rust 3-attempt
// loop with exponential backoff.
const VaultSearchRetryAttempts = 3

// vaultSearchBackoffsMS holds the inter-attempt delays in milliseconds. The
// list has VaultSearchRetryAttempts-1 entries — last attempt has no follow-up.
var vaultSearchBackoffsMS = []int{200, 600}

// HandleVaultSearch implements the vault_search MCP action.
//
// Required param: query (the task description / search text).
// Optional params: top_k (default 5, clamped to [1, 20]), vault_root (test override).
func HandleVaultSearch(ctx context.Context, deps Deps, params json.RawMessage) (VaultSearchResult, error) {
	query := mcpparam.String(params, "query")
	if query == "" {
		return VaultSearchResult{Error: "params.query is required (the task description / search text)"}, nil
	}
	topK := mcpparam.Int64(params, "top_k", 5)
	if topK < 1 {
		topK = 1
	}
	if topK > 20 {
		topK = 20
	}
	rootOverride := mcpparam.String(params, "vault_root")
	if rootOverride == "" {
		rootOverride = deps.VaultRoot
	}

	root, err := vault.ResolveRoot(rootOverride)
	if err != nil {
		return VaultSearchResult{Error: fmt.Sprintf("vault root: %s", err.Error())}, nil
	}

	entries, err := vault.Walk(root)
	if err != nil {
		return VaultSearchResult{Error: fmt.Sprintf("vault walk: %s", err.Error())}, nil
	}

	cap := effectiveMaxCandidates()
	filtered := vault.KeywordPrefilter(entries, query, cap)
	candidates := make([]qwenretrieve.RetrieveCandidate, len(filtered))
	for i, e := range filtered {
		candidates[i] = vaultEntryToCandidate(e)
	}

	// Bound the pass-1 prompt to Qwen's context window by TOKEN size, not just
	// candidate count: the fixed count cap overflows the 8192 window on wordy
	// candidate sets (bug 951). Candidates are keyword-ranked best-first, so
	// trimming the tail drops the least-relevant.
	candidates, _ = qwenretrieve.BudgetPass1Candidates(
		qwenretrieve.RetrieveTaskInput{Query: query, TopK: int(topK)},
		candidates, qwenretrieve.CorpusShapeVault, qwenretrieve.Pass1TokenBudget(),
	)

	bodyProvider := qwenretrieve.BodyExcerptProvider(func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			body, err := vault.ReadNoteBodyExcerpt(root, p, Pass2BodyExcerptChars)
			if err != nil {
				continue
			}
			out[i] = body
		}
		return out
	})

	// Cold-start retry: pass-1 EmptyResponse-style errors are retried with
	// 200ms then 600ms backoffs. Non-empty inference errors surface immediately.
	ctx = qwenctx.WithTaskID(ctx, "vault-rerank-retrieve")
	var result qwenretrieve.TwoPassRetrieveResult
	var lastErr error
	for attempt := 0; attempt < VaultSearchRetryAttempts; attempt++ {
		r, err := qwenretrieve.DispatchTwoPassRetrieve(
			ctx, deps.Router, query, int(topK),
			candidates, qwenretrieve.CorpusShapeVault, bodyProvider,
		)
		if err == nil {
			result = r
			lastErr = nil
			break
		}
		lastErr = err
		// Reactive safety net (bug 951): if the upfront token budget under-
		// estimated and pass-1 still overflowed the context window, halve the
		// candidate set and retry immediately (no backoff) rather than surfacing
		// the hard "Context size has been exceeded" 500.
		if qwenretrieve.IsContextExceededError(err) {
			if len(candidates) <= 1 {
				break
			}
			candidates = candidates[:len(candidates)/2]
			continue
		}
		// EmptyResponse is reported by llamacpp as "empty choices in response".
		// Only that specific transient is retryable; other errors propagate.
		if !isTransientInferenceError(err) || attempt == VaultSearchRetryAttempts-1 {
			break
		}
		delay := vaultSearchBackoffsMS[attempt]
		select {
		case <-time.After(time.Duration(delay) * time.Millisecond):
		case <-ctx.Done():
			return VaultSearchResult{Error: fmt.Sprintf("inference: %s", ctx.Err().Error())}, nil
		}
	}
	if lastErr != nil {
		if isTransientInferenceError(lastErr) {
			return VaultSearchResult{
				Error:     fmt.Sprintf("inference: %s", lastErr.Error()),
				Transient: true,
				Hint:      "Retry the query — Qwen cold-start; typically resolves within a few seconds",
			}, nil
		}
		return VaultSearchResult{Error: fmt.Sprintf("inference: %s", lastErr.Error())}, nil
	}

	ranked := wrapParsedPaths(result.RankedPaths, entries, int(topK))
	totalLatencyMS := result.Pass1LatencyMS
	if result.Pass2LatencyMS != nil {
		totalLatencyMS += *result.Pass2LatencyMS
	}
	totalInputTokens := jsonutil.SumOptInt64(result.Pass1InputTokens, result.Pass2InputTokens)
	totalOutputTokens := jsonutil.SumOptInt64(result.Pass1OutputTokens, result.Pass2OutputTokens)

	// Vault is cross-project; project_id on the grounding_events row
	// is left empty by convention (the table's project_id column is
	// NOT NULL, so we pass "" rather than nil). Per-handler telemetry
	// (pass1/pass2 latency) lands inline on the grounding_events row —
	// the legacy vault_search_invocations table was retired by chain
	// telemetry-substrate-cleanup T2 (migration 046).
	pass1Latency := result.Pass1LatencyMS
	recordGroundingEvent(ctx, deps.Pool, "", "vault_search", query, int64(len(ranked)), groundingRefsFromVault(ranked), HandlerTelemetry{
		Pass1LatencyMS: &pass1Latency,
		Pass2LatencyMS: result.Pass2LatencyMS,
	})

	resp := VaultSearchResult{
		Results:       ranked,
		VaultRoot:     root,
		VaultSize:     len(entries),
		LatencyMS:     totalLatencyMS,
		InputTokens:   totalInputTokens,
		OutputTokens:  totalOutputTokens,
		Pass2FellBack: result.FellBack,
	}
	candidatesUsed := len(candidates)
	if len(entries) > candidatesUsed {
		resp.CandidatesTruncated = true
		resp.CandidatesUsed = candidatesUsed
		resp.TruncatedNoteCount = len(entries) - candidatesUsed
		resp.CandidatesHint = fmt.Sprintf(
			"vault has %d notes; the %d with the highest keyword-overlap score that fit Qwen's context window were sent to rerank (the remaining %d were excluded — by the candidate cap and/or the pass-1 token budget). "+
				"Scoring runs over path / title / tags / summary / body (first ~4 KB), so older notes whose body matches the query DO surface in pass-1 — older does not mean excluded. "+
				"If no entry scores > 0 the prefilter falls back to walk order (path-alphabetical); rephrase the query if results look stale. "+
				"To raise the cap, set %s=<N> (count) or %s=<N> (Qwen context tokens) and restart the daemon; current cap is %d, default is %d, context %d tokens.",
			len(entries), candidatesUsed, len(entries)-candidatesUsed,
			MaxVaultCandidatesEnvVar, qwenretrieve.QwenContextTokensEnvVar, cap, MaxVaultCandidates, qwenretrieve.QwenContextTokens,
		)
	}
	return resp, nil
}

// HandleVaultRead implements the vault_read MCP action.
//
// Required param: path (relative to the vault root, as returned by vault_search).
// Optional param: vault_root (test override).
//
// Response shape: NoteContent fields plus edit_hint naming the absolute on-disk
// path. The harness's Edit tool requires a Read tool call on the path before
// it accepts an Edit; surfacing the hint here removes the surprise on first hit.
func HandleVaultRead(ctx context.Context, deps Deps, params json.RawMessage) (VaultReadResult, error) {
	_ = ctx
	path := mcpparam.String(params, "path")
	if path == "" {
		return VaultReadResult{Error: "params.path is required (relative to the vault root, as returned by vault_search)"}, nil
	}
	rootOverride := mcpparam.String(params, "vault_root")
	if rootOverride == "" {
		rootOverride = deps.VaultRoot
	}
	root, err := vault.ResolveRoot(rootOverride)
	if err != nil {
		return VaultReadResult{Error: fmt.Sprintf("vault root: %s", err.Error())}, nil
	}
	note, err := vault.ReadNote(root, path)
	if err != nil {
		switch {
		case errors.Is(err, vault.ErrPathTraversal):
			return VaultReadResult{
				Error: "path_traversal",
				Hint:  "vault_read accepts paths relative to the vault root only; absolute paths and `..` segments that escape the root are rejected",
			}, nil
		case errors.Is(err, vault.ErrNoteNotFound):
			return VaultReadResult{
				Error: "note_not_found",
				Path:  path,
			}, nil
		default:
			return VaultReadResult{Error: fmt.Sprintf("vault: %s", err.Error())}, nil
		}
	}
	return buildVaultReadResponse(note, root), nil
}

// buildVaultReadResponse mirrors Rust's build_vault_read_response — serialise
// the note and append an `edit_hint` that names the absolute on-disk path.
func buildVaultReadResponse(note vault.NoteContent, root string) VaultReadResult {
	absPath := filepath.Join(root, note.Path)
	return VaultReadResult{
		Path:               note.Path,
		Frontmatter:        note.Frontmatter,
		Content:            note.Content,
		EditHint:           fmt.Sprintf("To edit this note, call the Read tool on %s before Edit. The agent harness's must-read-first guard only tracks Read calls, not vault_read.", absPath),
		FrontmatterWarning: note.FrontmatterWarning,
	}
}

// vaultEntryToCandidate converts a vault.Entry into the dispatcher's
// RetrieveCandidate shape. Empty strings collapse to nil so the prompt builder
// omits the line.
func vaultEntryToCandidate(e vault.Entry) qwenretrieve.RetrieveCandidate {
	var title *string
	if e.Title != "" {
		t := e.Title
		title = &t
	}
	var summary *string
	if e.Summary != "" {
		s := e.Summary
		summary = &s
	}
	return qwenretrieve.RetrieveCandidate{
		Path:    e.Path,
		Title:   title,
		Tags:    append([]string(nil), e.Tags...),
		Summary: summary,
	}
}

// wrapParsedPaths wraps each parsed path into the {path, title, tags, rank}
// JSON shape the MCP response carries. Looks up entries by path so the title
// and tags are attached. Caps at topK rows. Unknown paths are skipped.
func wrapParsedPaths(paths []string, entries []vault.Entry, topK int) []VaultResultEntry {
	byPath := make(map[string]vault.Entry, len(entries))
	for _, e := range entries {
		byPath[e.Path] = e
	}
	out := make([]VaultResultEntry, 0, len(paths))
	for i, p := range paths {
		if i >= topK {
			break
		}
		e, ok := byPath[p]
		if !ok {
			continue
		}
		out = append(out, VaultResultEntry{
			Path:  p,
			Title: e.Title,
			Tags:  e.Tags,
			Rank:  i + 1,
		})
	}
	return out
}

// isTransientInferenceError reports whether err is the empty-response cold-start
// failure that warrants a retry. The llamacpp client surfaces empty choices as
// "router generate: ... empty choices in response".
func isTransientInferenceError(err error) bool {
	if err == nil {
		return false
	}
	return stringutil.ContainsCaseInsensitive(err.Error(), "empty choices in response")
}
