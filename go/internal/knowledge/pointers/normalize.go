package pointers

import "strings"

// vaultLegacyPrefix is the historic ".claude/vault/" prefix the Rust
// knowledge_seeder used when writing vault entries into the
// knowledge_pointers table. Migration 050 strips it from every existing
// row; this constant lives here so future code that re-encounters the
// legacy form has one place to defang it.
const vaultLegacyPrefix = ".claude/vault/"

// NormalizeVaultSourceRef returns the canonical bare form for a vault
// source_ref ("<subdir>/<file>.md"). Closes bug 1469's
// vault-pointer-prefix drift: two writer paths into knowledge_pointers
// historically used different prefix conventions (forge/indexsync.go
// emitted bare; the archived knowledge_seeder emitted prefixed). The
// canonical shape is bare per the bug's recommendation — shorter, and
// matches what `filepath.Rel(vaultRoot, abs)` naturally produces.
//
// Callers that build a KnowledgePointer with source_type="vault" should
// pass the source_ref through this function before insert. The function
// is idempotent: bare input is returned unchanged.
func NormalizeVaultSourceRef(ref string) string {
	return strings.TrimPrefix(ref, vaultLegacyPrefix)
}
