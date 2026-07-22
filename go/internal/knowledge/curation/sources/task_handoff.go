// Package sources holds the per-origin SourceMaterialBuilder implementations
// for the curation chain. See docs/CURATION_GO_MIGRATION.md §5.
//
// Builders are stateless (read-only after construction); the registry
// in package curation shares one instance across all passes.
package sources

import (
	"context"
	"fmt"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
)

// TaskHandoffBuilder builds source material for candidates with
// origin='task_handoff'. Reads tasks.problem_statement + tasks.handoff_output
// for the task whose slug appears in source_ref (after the "::" separator).
//
// Mirrors the source-material assembly in benchmarks/src/bin/knowledge_curate.rs
// secondary_pass.
type TaskHandoffBuilder struct{}

func NewTaskHandoffBuilder() *TaskHandoffBuilder {
	return &TaskHandoffBuilder{}
}

func (TaskHandoffBuilder) Origin() string { return "task_handoff" }

func (TaskHandoffBuilder) Build(ctx context.Context, pool *db.Pool, cand curation.Candidate) (string, error) {
	slug, ok := splitSourceRef(cand.SourceRef)
	if !ok {
		return "", fmt.Errorf("task_handoff build: source_ref %q missing %q separator",
			cand.SourceRef, "::")
	}

	var problemStatement string
	var handoffOutput string
	err := pool.DB().QueryRowContext(ctx,
		`SELECT problem_statement, COALESCE(handoff_output, '')
		   FROM proj_current_tasks WHERE slug = ?`,
		slug,
	).Scan(&problemStatement, &handoffOutput)
	if err != nil {
		return "", fmt.Errorf("task_handoff build: task %q: %w", slug, err)
	}

	excerpt := truncateRunes(handoffOutput, curation.ExcerptChars)
	return fmt.Sprintf("%s\n\nHandoff:\n%s", problemStatement, excerpt), nil
}

// splitSourceRef parses "<project>::<slug>" into the slug part. Returns
// ("", false) if the separator is missing.
func splitSourceRef(sourceRef string) (string, bool) {
	_, slug, found := strings.Cut(sourceRef, "::")
	if !found || slug == "" {
		return "", false
	}
	return slug, true
}

// truncateRunes caps s at n runes (not bytes — matches Rust's
// chars().take(n) behavior used in the original source assembly).
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
