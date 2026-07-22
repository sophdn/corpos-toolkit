package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"toolkit/internal/knowledge/vault"
	"toolkit/internal/mcpparam"
)

// memoryKinds is the closed memory taxonomy, each a subdirectory under
// <vault>/memory/. The subdirectory IS the kind (more reliable than the
// frontmatter `metadata.type`, which it mirrors). Routing (docs/MEMORY_SUBSTRATE.md
// §4): `user` fans out to EVERY project; `feedback`/`project`/`reference` are
// scoped to the entry's metadata.project.
var memoryKinds = []string{"user", "feedback", "project", "reference"}

// memoryFrontmatter is the subset of a memory entry's YAML frontmatter the
// digest needs. The project lives nested under `metadata:` (forge stamps it from
// the write envelope), which the flat vault.Frontmatter does not capture — hence
// this memory-specific shape.
type memoryFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Metadata    struct {
		Type    string `yaml:"type"`
		Project string `yaml:"project"`
	} `yaml:"metadata"`
}

// memoryEntry is one materialized entry headed for the digest.
type memoryEntry struct {
	Kind        string
	Name        string
	Description string
	Project     string
	RelPath     string // relative to the vault root, forward-slash normalized
}

// MemoryReadResult is the materialized memory digest for a project — the owned
// read path the corpos SessionStart hook injects (chain toolkit-decomposition
// T5). MemoryMarkdown is the bullet digest (one line per entry); Entries is the
// structured form for callers that want to render their own.
type MemoryReadResult struct {
	Project        string        `json:"project"`
	EntryCount     int           `json:"entry_count"`
	MemoryMarkdown string        `json:"memory_markdown"`
	Entries        []memoryEntry `json:"entries,omitempty"`
	Error          string        `json:"error,omitempty"`
}

// HandleMemoryRead returns the materialized memory digest for a project: every
// `user`-kind entry (fanned out to all projects) plus the `feedback`/`project`/
// `reference` entries scoped to this project (an entry with no metadata.project
// is treated as a fallback that materializes everywhere, matching the
// materialize-memory hook). Read-only; degrades gracefully (a malformed or
// unreadable entry is skipped, never fatal).
//
// Params: project (required). vault_root (optional override; tests inject a temp dir).
func HandleMemoryRead(ctx context.Context, deps Deps, params json.RawMessage) (MemoryReadResult, error) {
	_ = ctx
	project := mcpparam.String(params, "project")
	if project == "" {
		return MemoryReadResult{Error: "params.project is required"}, nil
	}
	rootOverride := mcpparam.String(params, "vault_root")
	if rootOverride == "" {
		rootOverride = deps.VaultRoot
	}
	root, err := vault.ResolveRoot(rootOverride)
	if err != nil {
		return MemoryReadResult{Error: fmt.Sprintf("vault root: %s", err.Error())}, nil
	}

	entries := readMemoryEntries(root, project)
	return MemoryReadResult{
		Project:        project,
		EntryCount:     len(entries),
		MemoryMarkdown: renderMemoryDigest(entries),
		Entries:        entries,
	}, nil
}

// readMemoryEntries walks <root>/memory/<kind>/*.md and returns the entries that
// route to the given project, sorted by name. Missing kind dirs are skipped; a
// missing memory dir yields an empty slice.
func readMemoryEntries(root, project string) []memoryEntry {
	var out []memoryEntry
	for _, kind := range memoryKinds {
		dir := filepath.Join(root, "memory", kind)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue // kind dir absent — nothing to add
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
				continue
			}
			abs := filepath.Join(dir, f.Name())
			raw, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			fm, ok := parseMemoryFrontmatter(string(raw))
			if !ok {
				continue // no/garbled frontmatter — skip, never fatal
			}
			if !routesToProject(kind, fm.Metadata.Project, project) {
				continue
			}
			name := fm.Name
			if name == "" {
				name = strings.TrimSuffix(f.Name(), ".md")
			}
			out = append(out, memoryEntry{
				Kind:        kind,
				Name:        name,
				Description: fm.Description,
				Project:     fm.Metadata.Project,
				RelPath:     filepath.ToSlash(filepath.Join("memory", kind, f.Name())),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].RelPath < out[j].RelPath
	})
	return out
}

// routesToProject applies the §4 routing: user-kind fans out to every project;
// the project-scoped kinds match on metadata.project, with an empty project
// treated as a materialize-everywhere fallback.
func routesToProject(kind, entryProject, wantProject string) bool {
	if kind == "user" {
		return true
	}
	return entryProject == "" || entryProject == wantProject
}

// parseMemoryFrontmatter extracts the leading `---` YAML block into a
// memoryFrontmatter. Returns ok=false when no frontmatter block is present or it
// fails to parse (caller skips the entry).
func parseMemoryFrontmatter(content string) (memoryFrontmatter, bool) {
	trimmed := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return memoryFrontmatter{}, false
	}
	nl := strings.IndexByte(trimmed, '\n')
	afterOpen := trimmed[nl+1:]
	end := strings.Index(afterOpen, "\n---")
	if end < 0 {
		return memoryFrontmatter{}, false
	}
	var fm memoryFrontmatter
	if err := yaml.Unmarshal([]byte(afterOpen[:end+1]), &fm); err != nil {
		return memoryFrontmatter{}, false
	}
	return fm, true
}

// renderMemoryDigest renders the entries as the materialized bullet digest — one
// line per entry, `- [name](relpath) — description` — mirroring the MEMORY.md
// block the materialize-memory hook emits. Empty input yields "".
func renderMemoryDigest(entries []memoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("- [")
		b.WriteString(e.Name)
		b.WriteString("](")
		b.WriteString(e.RelPath)
		b.WriteString(")")
		if e.Description != "" {
			b.WriteString(" — ")
			b.WriteString(e.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}
