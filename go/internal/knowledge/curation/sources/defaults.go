package sources

import "toolkit/internal/knowledge/curation"

// DefaultBuilders returns the three production SourceMaterialBuilder
// impls this chain ships, in registration order. Binaries (curate-rescore,
// curate-discover, curate-seed) call this to populate their registry at
// startup instead of registering each by name.
//
// vaultRoot defaults to $HOME/.claude/vault when empty.
// projectsRoot defaults to $HOME/.claude/projects when empty.
//
// To add a new origin: implement curation.SourceMaterialBuilder in a new
// file in this package, append it to the slice returned here, and add
// the origin string to the valid-origins enum check that lands with T6.
// See docs/CURATION_GO_MIGRATION.md §8 (new-origin recipe).
func DefaultBuilders(vaultRoot, projectsRoot string) []curation.SourceMaterialBuilder {
	return []curation.SourceMaterialBuilder{
		NewTaskHandoffBuilder(),
		NewVaultNoteBuilder(vaultRoot),
		NewZeroResultGapBuilder(projectsRoot),
	}
}
