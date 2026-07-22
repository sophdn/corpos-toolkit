package sources

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/knowledge/curation"
)

// VaultNoteBuilder builds source material for candidates with
// origin='session_mining' and source_type='vault'. Reads the file at
// cand.SourceRef under the configured vault root.
//
// Replaces the named-vault subset of the Rust knowledge_seeder. The
// deprecated session-deep / session-question / web-conv paths are NOT
// served by this builder — those were the noise sources we bulk-rejected
// during 2026-05-17 triage (see bug knowledge-seeder-session-mining-
// captures-one-time-prompts-as-knowledge).
type VaultNoteBuilder struct {
	rootDir string
}

// NewVaultNoteBuilder roots at vaultRoot (typically ~/.claude/vault).
// Empty vaultRoot defaults to $HOME/.claude/vault.
func NewVaultNoteBuilder(vaultRoot string) *VaultNoteBuilder {
	if vaultRoot == "" {
		if home := os.Getenv("HOME"); home != "" {
			vaultRoot = filepath.Join(home, ".claude", "vault")
		}
	}
	return &VaultNoteBuilder{rootDir: vaultRoot}
}

func (VaultNoteBuilder) Origin() string { return "session_mining" }

func (b VaultNoteBuilder) Build(_ context.Context, _ *db.Pool, cand curation.Candidate) (string, error) {
	// source_ref shape: ".claude/vault/<subdir>/<file>.md" (relative path).
	rel := strings.TrimPrefix(cand.SourceRef, ".claude/vault/")
	if rel == cand.SourceRef {
		// No prefix to trim; treat source_ref as already vault-relative.
		// Belt-and-suspenders: also reject anything that looks like an
		// absolute path or contains a path-traversal segment.
		if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
			return "", fmt.Errorf("vault_note build: source_ref %q is not vault-relative", cand.SourceRef)
		}
	}
	if strings.Contains(rel, "..") {
		return "", fmt.Errorf("vault_note build: source_ref %q contains path traversal", cand.SourceRef)
	}

	abs := filepath.Join(b.rootDir, rel)
	body, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("vault_note build: read %s: %w", abs, err)
	}
	return truncateRunes(string(body), curation.ExcerptChars), nil
}
