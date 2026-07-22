// Command action-docs-corpus-gen regenerates the GENERATED action-doc corpora
// (go/internal/actiondocs/corpus/<surface>/*.toml) from each surface's co-located
// descriptor registry. The regenerated files are go:embed'd into the binary at
// build time.
//
// Go is the single source of truth for a generated surface's action docs: the
// TOML corpus is a GENERATED artifact, not hand-authored. Each surface's registry
// (work.HandleWorkActions over work's actionRegistry; knowledge.KnowledgeActionSpecs
// over knowledge's registry) carries the typed param shape + every surface-doc
// field (purpose, param aliases, value aliases, errors, notes, envelope
// requirements, examples, returns); this command projects each spec into an
// actiondocs.ActionDoc (actiondocs.SpecToDoc) and emits the matching TOML chunk.
// The generated corpus is what admin.action_describe(surface, ...) serves.
//
// Generated surfaces: work (chain single-source-action-describe →
// establish-action-doc-contract-on-work), knowledge (chain
// migrate-knowledge-action-docs-to-derive-contract), measure (chain
// migrate-measure-action-docs-to-derive-contract), admin (chain
// migrate-admin-action-docs-to-derive-contract), and ml (chain
// migrate-ml-action-docs-to-derive-contract). Only each surface's
// <surface>/_general.toml stays hand-authored and is left untouched.
//
// Usage:
//
//	action-docs-corpus-gen          # regenerate every generated surface's corpus in place
//	action-docs-corpus-gen --check  # exit non-zero if any on-disk chunk drifts
//	action-docs-corpus-gen --stdout # print one action's TOML and exit (debug)
//
// The --check mode is the no-diff gate the precommit hook wires: a drift means
// someone hand-edited a generated chunk or changed a registry without regenerating.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"toolkit/internal/actiondocs"
	"toolkit/internal/actionspec"
	"toolkit/internal/admin"
	"toolkit/internal/ecosystem"
	"toolkit/internal/fs"
	"toolkit/internal/knowledge"
	"toolkit/internal/measure"
	"toolkit/internal/ml"
	"toolkit/internal/sys"
	"toolkit/internal/work"
)

// surfaceGen pairs a generated surface's name with its derived action catalog.
type surfaceGen struct {
	name  string
	specs []actionspec.ActionSpec
}

// generatedSurfaces returns every surface whose corpus is generated from a
// registry, in a stable order. Adding a migrated surface is one entry here.
func generatedSurfaces() ([]surfaceGen, error) {
	ws, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		return nil, fmt.Errorf("work specs: %w", err)
	}
	return []surfaceGen{
		{name: "work", specs: []actionspec.ActionSpec(ws)},
		{name: "knowledge", specs: knowledge.KnowledgeActionSpecs()},
		{name: "measure", specs: measure.MeasureActionSpecs()},
		{name: "admin", specs: admin.AdminActionSpecs()},
		{name: "ml", specs: ml.MLActionSpecs()},
		{name: "fs", specs: fs.FsActionSpecs()},
		{name: "sys", specs: sys.SysActionSpecs()},
		{name: "ecosystem", specs: ecosystem.EcosystemActionSpecs()},
	}, nil
}

func main() {
	var (
		check  bool
		stdout string
	)
	flag.BoolVar(&check, "check", false, "Exit non-zero if any on-disk chunk differs from the regenerated output (no-diff gate).")
	flag.StringVar(&stdout, "stdout", "", "Print the named action's generated TOML to stdout and exit (debug); does not write.")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		log.Fatalf("action-docs-corpus-gen: %v", err)
	}

	surfaces, err := generatedSurfaces()
	if err != nil {
		log.Fatalf("action-docs-corpus-gen: load specs: %v", err)
	}

	// Debug: print one action's TOML and exit (search every generated surface).
	if stdout != "" {
		for _, sg := range surfaces {
			for _, s := range sg.specs {
				if s.Name == stdout {
					out, err := encodeDoc(actiondocs.SpecToDoc(sg.name, s))
					if err != nil {
						log.Fatalf("action-docs-corpus-gen: encode %s: %v", stdout, err)
					}
					os.Stdout.Write(out)
					return
				}
			}
		}
		log.Fatalf("action-docs-corpus-gen: no generated action named %q", stdout)
	}

	var anyDrift bool
	for _, sg := range surfaces {
		dir := filepath.Join(root, "go", "internal", "actiondocs", "corpus", sg.name)
		generated := generateSurface(sg, dir)
		if check {
			if surfaceDrift(sg.name, dir, generated) {
				anyDrift = true
			}
			continue
		}
		for name, out := range generated {
			path := filepath.Join(dir, name+".toml")
			if err := os.WriteFile(path, out, 0o644); err != nil {
				log.Fatalf("action-docs-corpus-gen: write %s: %v", path, err)
			}
		}
	}
	if check && anyDrift {
		fmt.Fprintln(os.Stderr, "Run scripts/action-docs-corpus-gen to regenerate and stage the result.")
		os.Exit(1)
	}
}

// generateSurface projects every spec for one surface into its TOML chunk in
// memory (all-or-nothing) and fails fast on an encode error or an orphan chunk
// (a chunk with no matching spec — the corpus must mirror the registry).
// <surface>/_general.toml is hand-authored cross-cutting prose, not a spec, and
// is exempt from the orphan check.
func generateSurface(sg surfaceGen, dir string) map[string][]byte {
	generated := make(map[string][]byte, len(sg.specs))
	specNames := make(map[string]bool, len(sg.specs))
	for _, s := range sg.specs {
		specNames[s.Name] = true
		out, err := encodeDoc(actiondocs.SpecToDoc(sg.name, s))
		if err != nil {
			log.Fatalf("action-docs-corpus-gen: encode %s/%s: %v", sg.name, s.Name, err)
		}
		generated[s.Name] = out
	}
	orphans, err := orphanChunks(dir, specNames)
	if err != nil {
		log.Fatalf("action-docs-corpus-gen: scan %s: %v", dir, err)
	}
	if len(orphans) > 0 {
		log.Fatalf("action-docs-corpus-gen: %d %s chunk(s) have no matching registry entry: %s\n"+
			"Either restore the descriptor or `git rm` the orphaned chunk(s); the corpus must mirror the registry.",
			len(orphans), sg.name, strings.Join(orphans, ", "))
	}
	return generated
}

// surfaceDrift reports (to stderr) any on-disk chunk that differs from the
// freshly generated bytes for one surface, returning true if any drifted.
func surfaceDrift(surface, dir string, generated map[string][]byte) bool {
	var drift []string
	for name, want := range generated {
		path := filepath.Join(dir, name+".toml")
		got, err := os.ReadFile(path)
		if err != nil {
			drift = append(drift, name+" (missing: "+err.Error()+")")
			continue
		}
		if !bytes.Equal(got, want) {
			drift = append(drift, name)
		}
	}
	if len(drift) == 0 {
		return false
	}
	sort.Strings(drift)
	fmt.Fprintf(os.Stderr, "action-docs-corpus-gen: %d %s chunk(s) drift from the registry:\n", len(drift), surface)
	for _, d := range drift {
		fmt.Fprintf(os.Stderr, "  - %s\n", d)
	}
	return true
}

// encodeDoc serializes an ActionDoc to TOML. BurntSushi emits all scalar
// keys (surface/action/purpose/notes) before any [[table]] array, which is
// both valid TOML and the correct placement — the prior hand-authored corpus
// put `notes` after [[params]]/[[errors]], where it was silently parsed into
// the last table element and dropped from describe.
func encodeDoc(doc actiondocs.ActionDoc) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// orphanChunks returns <dir>/*.toml basenames (minus the .toml suffix) that
// have no matching spec. <surface>/_general.toml is hand-authored cross-cutting
// prose and is exempt.
func orphanChunks(dir string, specNames map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".toml")
		if name == "_general" || specNames[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	return orphans, nil
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
