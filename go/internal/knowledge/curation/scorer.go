package curation

import (
	"context"
	"fmt"

	"toolkit/internal/inference/llamacpp"
)

// Scorer abstracts the Qwen-backed extraction + scoring pipeline so
// passes can be tested against a mock and live against llamacpp through
// the same surface. See docs/CURATION_GO_MIGRATION.md §3.
//
// Health() returning nil is the contract that gates abort-on-unreachable
// semantics in §6: every pass must call Health() once before any
// candidate work and bail with the returned error if it's non-nil.
type Scorer interface {
	// Extract generates retrieval metadata from source material.
	// Returns an error if generate fails or the response is unparseable.
	// Implementations MUST NOT silently fall back to templated metadata
	// — surface the error so callers can choose to skip vs abort.
	Extract(ctx context.Context, sourceType, sourceRef, sourceMaterial string) (ExtractedMeta, error)

	// Score returns an adversarial relevance score in [0.0, 1.0]. The
	// description is intentionally NOT passed — it would inflate scores
	// because the scorer would grade what it itself wrote.
	Score(ctx context.Context, question, sourceMaterial string) (float64, error)

	// Health returns nil iff the underlying inference endpoint is
	// reachable AND responding 2xx. Typed error names the URL + cause.
	Health(ctx context.Context) error
}

// QwenScorer is the production Scorer. Wraps a *llamacpp.Client and
// delegates extraction + scoring to the package-level helpers
// (QwenExtract / QwenScore) that hold the byte-identical prompts.
//
// Accept interfaces, return structs (go-conventions): constructors hand
// back the concrete type; callers that need substitutability take
// Scorer.
type QwenScorer struct {
	client *llamacpp.Client
}

// NewQwenScorer wraps client. Use llamacpp.NewFromEnv() to honor the
// TOOLKIT_LOCAL_URL canonical-URL contract.
func NewQwenScorer(client *llamacpp.Client) *QwenScorer {
	return &QwenScorer{client: client}
}

// Extract delegates to QwenExtract.
func (s *QwenScorer) Extract(
	ctx context.Context, sourceType, sourceRef, sourceMaterial string,
) (ExtractedMeta, error) {
	return QwenExtract(ctx, s.client, sourceType, sourceRef, sourceMaterial)
}

// Score delegates to QwenScore.
func (s *QwenScorer) Score(
	ctx context.Context, question, sourceMaterial string,
) (float64, error) {
	return QwenScore(ctx, s.client, question, sourceMaterial)
}

// Health delegates to the underlying client. Returns the client's typed
// *llamacpp.UnreachableError on failure so callers can pattern-match if
// they want to (errors.As).
func (s *QwenScorer) Health(ctx context.Context) error {
	if err := s.client.Health(ctx); err != nil {
		return fmt.Errorf("scorer health: %w", err)
	}
	return nil
}

// BaseURL surfaces the configured llamacpp URL for diagnostic output
// (the "we tried X" answer in error messages).
func (s *QwenScorer) BaseURL() string {
	return s.client.BaseURL()
}
