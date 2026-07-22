package refresolve

import (
	"toolkit/internal/db"
	"toolkit/internal/knowledge"
)

// ProductionDeps bundles every dependency the production resolver
// set needs. Construct one at server startup and pass to
// BuildProductionRegistry. Tests construct registries directly via
// NewRegistry + Register and skip this builder.
type ProductionDeps struct {
	// Pool is the toolkit-server DB pool (chains / tasks / bugs /
	// library entries). Required.
	Pool *db.Pool
	// Project is the project scope for library resolution (library
	// entries are project-scoped).
	Project string
	// KnowledgeDeps carries the knowledge meta-tool's dependencies
	// (router for Qwen rerank, vault root, kiwix base URL).
	// Required.
	KnowledgeDeps knowledge.Deps
	// RepoRoot points at the toolkit-server checkout for filesystem
	// resolvers (skill/tool/schema lookups and the path resolver's
	// repo-relative fallback). Empty string disables those
	// resolvers; the registry installs only the DB- and knowledge-
	// backed shapes.
	RepoRoot string
	// MemoryDir is the per-project auto-memory directory the
	// memory_entry resolver reads MEMORY.md from. Empty string
	// keeps the resolver in shell mode (TierNoHit always).
	// Reference-resolution-migration T10.
	MemoryDir string
}

// BuildProductionRegistry wires every resolver implemented in this
// package against the supplied dependencies. Returns a Registry
// ready for Dispatch.
//
// Resolvers with unmet dependencies (e.g., nil Pool) are silently
// omitted — the dispatcher will report TierNoHit + a "no resolver
// registered" error for any Reference whose Shape lacks a
// registered resolver. This lets partial wiring (e.g., a smoke
// harness with no real router) still exercise the rule-based
// resolvers.
func BuildProductionRegistry(deps ProductionDeps) *Registry {
	r := NewRegistry()

	if deps.Pool != nil {
		r.Register(chainResolver{pool: deps.Pool})
		r.Register(taskResolver{pool: deps.Pool})
		r.Register(bugResolver{pool: deps.Pool})
		r.Register(libraryResolver{pool: deps.Pool})
		// chain 435: deterministic local-ecosystem access answer, surfaced at
		// parse_context orient-time via ShapeEcosystemToken.
		r.Register(NewEcosystemResolver(deps.Pool))
		// canon_resolve: current canonical identity for a name/alias/path/port.
		r.Register(NewCanonResolver(deps.Pool))
	}
	if deps.RepoRoot != "" {
		r.Register(pathResolver{repoRoot: deps.RepoRoot})
		r.Register(skillResolver{repoRoot: deps.RepoRoot})
		r.Register(toolResolver{repoRoot: deps.RepoRoot})
		r.Register(schemaResolver{repoRoot: deps.RepoRoot})
		// reference-resolution-migration T5: skill_trigger resolver
		// reads skills/_manifest.toml at startup; absent manifest is
		// fine — the resolver returns TierNoHit until the manifest
		// lands. discipline_skill resolver reuses the same manifest.
		manifest, _ := LoadSkillManifest(deps.RepoRoot)
		r.Register(skillTriggerResolver{manifest: manifest})
		r.Register(disciplineSkillResolver{manifest: manifest})
		// Weak-boundary sibling — shares the manifest with the strict
		// resolver; emits ShapeSkillCandidate refs from detectSkillCandidate.
		r.Register(NewSkillCandidateResolver(manifest))
	}
	r.Register(projectResolver{})
	// Friction-shape resolver returns a filing-suggestion rather
	// than a binding (T6 supersession of the friction-filing-reminder
	// Stop hook). No external deps — always registered.
	r.Register(frictionResolver{})

	// Knowledge-backed resolvers require the knowledge.Deps
	// surface; gate on KnowledgeDeps.Pool which is the required
	// minimum.
	if deps.KnowledgeDeps.Pool != nil {
		r.Register(domainTermResolver{deps: deps.KnowledgeDeps, project: deps.Project})
		r.Register(externalTechnicalResolver{deps: deps.KnowledgeDeps, project: deps.Project})
		// reference-resolution-migration T5: vault + kiwix bridges
		// route through knowledge.{HandleVaultSearch, HandleKnowledgeSearch}
		// so the per-corpus search work re-uses the existing handlers
		// rather than reimplementing them in refresolve/.
		r.Register(NewVaultCandidateResolver(deps.KnowledgeDeps))
		r.Register(NewKiwixBridgeResolver(deps.KnowledgeDeps, deps.Project))
	}

	// reference-resolution-migration T10: memory_entry resolver
	// reads MEMORY.md from deps.MemoryDir and maps hyphenated
	// identifiers to entries. Empty MemoryDir registers the shell
	// (TierNoHit always); set the dir to e.g.
	// ~/.claude/projects/<cwd-slug>/memory/ to wire the real lookup.
	var memIndex *MemoryIndex
	if deps.MemoryDir != "" {
		if idx, err := LoadMemoryIndex(deps.MemoryDir); err == nil {
			memIndex = idx
		}
	}
	r.Register(NewMemoryEntryResolver(memIndex))

	return r
}
