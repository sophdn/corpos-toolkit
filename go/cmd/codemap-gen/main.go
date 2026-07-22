// Command codemap-gen emits CODEMAP.md at the repo root from the
// authoritative discovery sources:
//
//   - action-manifests/*.toml      → meta-tool action catalogue
//   - action-manifests/dispatch-policy.toml → rationale-required flag
//   - blueprints/forge-schemas/*.toml → forge schema catalogue
//   - go/internal/*/doc.go         → Go package intended-use blocks
//   - crates/*/src/lib.rs (and a few sibling roots) → Rust crate intended-use
//   - skills/*.toml                → skill catalogue
//   - scripts/*                    → script catalogue
//
// All inputs are enumerated by filesystem glob; new packages, manifests,
// or skills surface in CODEMAP.md without any code change here. Output
// is deterministic (sorted) so two runs against the same tree produce
// byte-identical files.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

func main() {
	var (
		check    bool
		stdout   bool
		lintOnly bool
	)
	flag.BoolVar(&check, "check", false, "Validate every go/internal/* doc.go has the four-field block AND CODEMAP.md is up to date. Exit non-zero on either failure.")
	flag.BoolVar(&stdout, "stdout", false, "Write generated content to stdout instead of CODEMAP.md.")
	flag.BoolVar(&lintOnly, "lint", false, "Validate every go/internal/* doc.go has the four-field block; skip the CODEMAP.md staleness check.")
	flag.Parse()

	root, err := repoRoot()
	if err != nil {
		log.Fatalf("codemap-gen: %v", err)
	}

	if lintOnly {
		if err := lintGoDocs(root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if check {
		if err := lintGoDocs(root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}

	generated, err := generate(root)
	if err != nil {
		log.Fatalf("codemap-gen: %v", err)
	}

	target := filepath.Join(root, "CODEMAP.md")

	if stdout {
		if _, err := os.Stdout.Write(generated); err != nil {
			log.Fatalf("codemap-gen: write stdout: %v", err)
		}
		return
	}

	if check {
		existing, err := os.ReadFile(target)
		if err != nil {
			log.Fatalf("codemap-gen: --check: read existing %s: %v\n(run scripts/codemap-gen to create it)", target, err)
		}
		if !bytes.Equal(existing, generated) {
			fmt.Fprintln(os.Stderr, "codemap-gen: CODEMAP.md is stale.")
			fmt.Fprintln(os.Stderr, "Run scripts/codemap-gen to regenerate and stage the result.")
			os.Exit(1)
		}
		return
	}

	if err := os.WriteFile(target, generated, 0o644); err != nil {
		log.Fatalf("codemap-gen: write %s: %v", target, err)
	}
}

// lintGoDocs validates every package under go/internal/ ships a doc.go
// containing the four-field `## Intended use` block. Returns a
// human-readable error listing every offending package, or nil on pass.
func lintGoDocs(root string) error {
	dirs, err := filepath.Glob(filepath.Join(root, "go", "internal", "*"))
	if err != nil {
		return fmt.Errorf("glob go/internal: %w", err)
	}
	sort.Strings(dirs)

	type problem struct {
		pkg    string
		path   string
		reason string
	}
	var problems []problem
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		pkg := filepath.Base(dir)
		// Skip namespace-only dirs that hold no top-level .go files (just
		// subpackages) — they aren't Go packages and need no doc.go. e.g.
		// go/internal/forge after chain 311 T7 Stage 6 P2-C.2 archived forge's
		// top-level source, leaving only the forge/registry + forge/fieldvalue
		// subpackages (each linted on its own merits when nested globs run).
		if goFiles, _ := filepath.Glob(filepath.Join(dir, "*.go")); len(goFiles) == 0 {
			continue
		}
		docPath := filepath.Join(dir, "doc.go")
		if _, err := os.Stat(docPath); os.IsNotExist(err) {
			problems = append(problems, problem{
				pkg:    pkg,
				path:   relPath(root, docPath),
				reason: "missing doc.go",
			})
			continue
		}
		block, missing := parseGoDocFile(docPath)
		if block == "" {
			problems = append(problems, problem{
				pkg:    pkg,
				path:   relPath(root, docPath),
				reason: "missing `## Intended use` heading in doc.go's leading comment",
			})
			continue
		}
		if len(missing) > 0 {
			problems = append(problems, problem{
				pkg:    pkg,
				path:   relPath(root, docPath),
				reason: fmt.Sprintf("doc.go intended-use block missing field(s): %s", strings.Join(missing, ", ")),
			})
		}
	}
	if len(problems) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("codemap-gen: Go package doc.go lint failed:\n")
	for _, p := range problems {
		fmt.Fprintf(&b, "  - %s (%s): %s\n", p.pkg, p.path, p.reason)
	}
	b.WriteString("\nSee CONVENTIONS.md §\"Intended use\" — every internal package needs:\n")
	b.WriteString("    // Package <name> ...\n")
	b.WriteString("    //\n")
	b.WriteString("    // ## Intended use\n")
	b.WriteString("    //\n")
	b.WriteString("    // **Workflow served:** ...\n")
	b.WriteString("    // **Invocation pattern:** ...\n")
	b.WriteString("    // **Success shape:** ...\n")
	b.WriteString("    // **Non-goals:** ...\n")
	b.WriteString("    package <name>\n")
	return fmt.Errorf("%s", b.String())
}

func repoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// generate produces the full CODEMAP.md body.
func generate(root string) ([]byte, error) {
	var buf bytes.Buffer

	writeHeader(&buf)

	metaTools, err := loadMetaTools(root)
	if err != nil {
		return nil, fmt.Errorf("meta-tools: %w", err)
	}
	writeMetaTools(&buf, metaTools)

	schemas, err := loadForgeSchemas(root)
	if err != nil {
		return nil, fmt.Errorf("forge schemas: %w", err)
	}
	writeForgeSchemas(&buf, schemas)

	goPackages, err := loadGoPackages(root)
	if err != nil {
		return nil, fmt.Errorf("go packages: %w", err)
	}
	writeGoPackages(&buf, goPackages)

	rustCrates, err := loadRustCrates(root)
	if err != nil {
		return nil, fmt.Errorf("rust crates: %w", err)
	}
	writeRustCrates(&buf, rustCrates)

	skills, err := loadSkills(root)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	writeSkills(&buf, skills)

	scripts, err := loadScripts(root)
	if err != nil {
		return nil, fmt.Errorf("scripts: %w", err)
	}
	writeScripts(&buf, scripts)

	return buf.Bytes(), nil
}

func writeHeader(buf *bytes.Buffer) {
	buf.WriteString(`<!-- GENERATED by scripts/codemap-gen — DO NOT EDIT by hand. -->
<!-- Regenerate with: scripts/codemap-gen. The precommit gate fails if this file is stale. -->

# CODEMAP — mcp-servers

Navigation entry point for the mcp-servers workspace. Sections below
are mechanically derived from the action manifests, forge schemas, Go
package doc.go files, Rust crate intended-use blocks, skill manifests,
and the scripts directory. An agent can decide whether to read a
package without first reading its code.

`)
}

// ─────────────────────────── meta-tools ──────────────────────────────

type actionEntry struct {
	Name              string // snake_case canonical action name
	Description       string // first-sentence description from action manifest
	RequiresRationale bool   // from dispatch-policy.toml
	Surface           string // work | knowledge | measure | admin | "uncategorized"
	ManifestPath      string // relative path
}

type dispatchPolicy map[string]map[string]struct {
	RequiresRationale bool `toml:"requires_rationale"`
}

type actionManifest struct {
	Skill struct {
		Name        string `toml:"name"`
		Description string `toml:"description"`
	} `toml:"skill"`
}

// loadMetaTools enumerates action-manifests/*.toml, joins each manifest
// against dispatch-policy.toml for its surface and rationale flag, and
// returns the result grouped by surface.
func loadMetaTools(root string) (map[string][]actionEntry, error) {
	policyPath := filepath.Join(root, "action-manifests", "dispatch-policy.toml")
	var policy dispatchPolicy
	if _, err := toml.DecodeFile(policyPath, &policy); err != nil {
		return nil, fmt.Errorf("decode dispatch-policy: %w", err)
	}

	// Build action→surface map from policy (mutating actions, authoritative).
	actionSurface := map[string]string{}
	rationaleFlag := map[string]bool{}
	for surface, actions := range policy {
		for name, cfg := range actions {
			actionSurface[name] = surface
			rationaleFlag[name] = cfg.RequiresRationale
		}
	}

	manifests, err := filepath.Glob(filepath.Join(root, "action-manifests", "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(manifests)

	grouped := map[string][]actionEntry{}
	for _, path := range manifests {
		base := filepath.Base(path)
		if base == "dispatch-policy.toml" {
			continue
		}
		var m actionManifest
		if _, err := toml.DecodeFile(path, &m); err != nil {
			return nil, fmt.Errorf("decode %s: %w", base, err)
		}
		name := strings.ReplaceAll(m.Skill.Name, "-", "_")
		surface, ok := actionSurface[name]
		if !ok {
			// Read-only action — not in dispatch-policy. Infer surface
			// from name prefix; if unknown, bucket as "uncategorized".
			surface = inferSurface(name)
		}
		entry := actionEntry{
			Name:              name,
			Description:       firstSentence(m.Skill.Description),
			RequiresRationale: rationaleFlag[name],
			Surface:           surface,
			ManifestPath:      filepath.Join("action-manifests", base),
		}
		grouped[surface] = append(grouped[surface], entry)
	}

	for _, list := range grouped {
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	}
	return grouped, nil
}

// inferSurface assigns a meta-tool surface to a read-only action whose
// name doesn't appear in dispatch-policy.toml. The mapping is by name
// prefix and is the only implicit data source in the codemap pipeline;
// new prefixes get added here, not in dispatch-policy.
func inferSurface(name string) string {
	switch {
	case strings.HasPrefix(name, "bug_"),
		strings.HasPrefix(name, "task_"),
		strings.HasPrefix(name, "chain_"),
		strings.HasPrefix(name, "roadmap_"),
		strings.HasPrefix(name, "forge"),
		strings.HasPrefix(name, "block_"),
		strings.HasPrefix(name, "complete_"),
		strings.HasPrefix(name, "cancel_"),
		strings.HasPrefix(name, "close_"),
		strings.HasPrefix(name, "find_"),
		strings.HasPrefix(name, "edit_"),
		strings.HasPrefix(name, "check_"):
		return "work"
	case strings.HasPrefix(name, "knowledge_"),
		strings.HasPrefix(name, "kiwix_"),
		strings.HasPrefix(name, "library_"):
		return "knowledge"
	case strings.HasPrefix(name, "benchmark_"),
		strings.HasPrefix(name, "classify"):
		return "measure"
	case strings.HasPrefix(name, "project_"),
		strings.HasPrefix(name, "host_"),
		strings.HasPrefix(name, "schema_"),
		strings.HasPrefix(name, "vault_"),
		strings.HasPrefix(name, "server_"),
		strings.HasPrefix(name, "remote_"),
		strings.HasPrefix(name, "health"):
		return "admin"
	}
	return "uncategorized"
}

func writeMetaTools(buf *bytes.Buffer, grouped map[string][]actionEntry) {
	buf.WriteString("## Meta-tools\n\n")
	buf.WriteString("Action manifests under `action-manifests/`. Mutating actions carry " +
		"`(rationale required)` per `dispatch-policy.toml` (chain " +
		"agent-first-substrate T3).\n\n")

	order := []string{"work", "knowledge", "measure", "admin", "uncategorized"}
	for _, surface := range order {
		actions := grouped[surface]
		if len(actions) == 0 {
			continue
		}
		fmt.Fprintf(buf, "### %s\n\n", surface)
		for _, a := range actions {
			rationale := ""
			if a.RequiresRationale {
				rationale = " *(rationale required)*"
			}
			fmt.Fprintf(buf, "- `%s` — %s%s\n", a.Name, a.Description, rationale)
		}
		buf.WriteString("\n")
	}
}

// ─────────────────────── forge schemas ────────────────────────────────

type forgeSchema struct {
	Name        string
	Prefix      string
	OutputDir   string
	Description string // derived from the file's leading comment block
	Path        string
}

func loadForgeSchemas(root string) ([]forgeSchema, error) {
	files, err := filepath.Glob(filepath.Join(root, "blueprints", "forge-schemas", "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	var out []forgeSchema
	for _, path := range files {
		raw := struct {
			Schema struct {
				Name      string `toml:"name"`
				Prefix    string `toml:"prefix"`
				OutputDir string `toml:"output_dir"`
			} `toml:"schema"`
		}{}
		if _, err := toml.DecodeFile(path, &raw); err != nil {
			return nil, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
		}
		desc, err := leadingCommentDescription(path, "#")
		if err != nil {
			return nil, err
		}
		out = append(out, forgeSchema{
			Name:        raw.Schema.Name,
			Prefix:      raw.Schema.Prefix,
			OutputDir:   raw.Schema.OutputDir,
			Description: desc,
			Path:        relPath(root, path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func writeForgeSchemas(buf *bytes.Buffer, schemas []forgeSchema) {
	buf.WriteString("## Forge schemas\n\n")
	buf.WriteString("TOML-declared artifact schemas under `blueprints/forge-schemas/`. " +
		"Each row's create / edit / delete path lives in `internal/forge`.\n\n")
	for _, s := range schemas {
		var meta []string
		if s.Prefix != "" {
			meta = append(meta, fmt.Sprintf("prefix `%s`", s.Prefix))
		}
		if s.OutputDir != "" {
			meta = append(meta, fmt.Sprintf("output `%s`", s.OutputDir))
		}
		metaStr := ""
		if len(meta) > 0 {
			metaStr = " — " + strings.Join(meta, ", ")
		}
		fmt.Fprintf(buf, "### `%s`%s\n\n", s.Name, metaStr)
		if s.Description != "" {
			buf.WriteString(s.Description)
			buf.WriteString("\n\n")
		}
	}
}

// ─────────────────────── Go packages ─────────────────────────────────

type goPackage struct {
	Name          string // last segment of path
	Path          string // relative to root, e.g. "go/internal/admin"
	IntendedUse   string // full block including the four fields
	HasFourFields bool
	MissingFields []string // which of the four canonical fields are missing
}

var goRequiredFields = []string{"Workflow served", "Invocation pattern", "Success shape", "Non-goals"}

func loadGoPackages(root string) ([]goPackage, error) {
	dirs, err := filepath.Glob(filepath.Join(root, "go", "internal", "*"))
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)

	var out []goPackage
	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		pkg := goPackage{
			Name: filepath.Base(dir),
			Path: relPath(root, dir),
		}
		docPath := filepath.Join(dir, "doc.go")
		block, missing := parseGoDocFile(docPath)
		pkg.IntendedUse = block
		pkg.MissingFields = missing
		pkg.HasFourFields = block != "" && len(missing) == 0
		out = append(out, pkg)
	}
	return out, nil
}

// parseGoDocFile reads doc.go and returns (intendedUseBlock, missingFields).
// If the file is missing or has no ## Intended use block, returns ("", allFields).
// Comment markers (`// `, `//`) are stripped.
func parseGoDocFile(path string) (string, []string) {
	f, err := os.Open(path)
	if err != nil {
		return "", append([]string(nil), goRequiredFields...)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "package ") {
			break
		}
		if strings.HasPrefix(line, "//") {
			content := strings.TrimPrefix(line, "//")
			content = strings.TrimPrefix(content, " ")
			lines = append(lines, content)
		}
	}

	// Find the ## Intended use heading and collect everything after it.
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Intended use" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return "", append([]string(nil), goRequiredFields...)
	}
	block := strings.Join(lines[start:], "\n")
	block = strings.TrimSpace(block)

	missing := missingFields(block, goRequiredFields)
	return block, missing
}

func missingFields(block string, required []string) []string {
	var missing []string
	for _, f := range required {
		// Match either `**Field:**` markdown form OR plain `Field:` form.
		needleBold := "**" + f + ":**"
		needlePlain := f + ":"
		if !strings.Contains(block, needleBold) && !strings.Contains(block, needlePlain) {
			missing = append(missing, f)
		}
	}
	return missing
}

func writeGoPackages(buf *bytes.Buffer, pkgs []goPackage) {
	buf.WriteString("## Go packages (`go/internal/`)\n\n")
	buf.WriteString("Per-package intended-use blocks from each package's `doc.go`. " +
		"The four-field block (Workflow served, Invocation pattern, Success shape, " +
		"Non-goals) is enforced by the precommit lint.\n\n")
	for _, p := range pkgs {
		fmt.Fprintf(buf, "### `%s` — `%s`\n\n", p.Name, p.Path)
		if p.IntendedUse == "" {
			buf.WriteString("*(no doc.go intended-use block)*\n\n")
			continue
		}
		buf.WriteString(p.IntendedUse)
		buf.WriteString("\n\n")
	}
}

// ─────────────────────── Rust crates ─────────────────────────────────

type rustCrate struct {
	Name        string
	Path        string
	IntendedUse string
}

func loadRustCrates(root string) ([]rustCrate, error) {
	// Enumerate crates/*/src/lib.rs and a few sibling-root crates that
	// historically live outside crates/ (benchmarks, inference-clients).
	var paths []string
	patterns := []string{
		filepath.Join(root, "crates", "*", "src", "lib.rs"),
		filepath.Join(root, "benchmarks", "src", "lib.rs"),
		filepath.Join(root, "inference-clients", "src", "lib.rs"),
	}
	for _, p := range patterns {
		matches, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)

	var out []rustCrate
	for _, path := range paths {
		block := parseRustIntendedUse(path)
		if block == "" {
			continue // Infrastructure-only crates are exempt; skip.
		}
		// crate name = directory two levels up from src/lib.rs
		crateDir := filepath.Dir(filepath.Dir(path))
		out = append(out, rustCrate{
			Name:        filepath.Base(crateDir),
			Path:        relPath(root, crateDir),
			IntendedUse: block,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseRustIntendedUse extracts the contents under `//! ## Intended use`
// from a Rust lib.rs. Comment markers (`//! `, `//!`) are stripped.
func parseRustIntendedUse(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lines []string
	inDoc := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "//!") {
			inDoc = true
			content := strings.TrimPrefix(line, "//!")
			content = strings.TrimPrefix(content, " ")
			lines = append(lines, content)
			continue
		}
		// Stop scanning once we've left the leading inner-doc block,
		// allowing intervening blank lines between attribute lines.
		if inDoc && line != "" && !strings.HasPrefix(line, "#") {
			break
		}
	}

	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Intended use" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	block := strings.Join(lines[start:], "\n")
	return strings.TrimSpace(block)
}

func writeRustCrates(buf *bytes.Buffer, crates []rustCrate) {
	buf.WriteString("## Rust crates\n\n")
	if len(crates) == 0 {
		buf.WriteString("*(no Rust crates currently carry a `## Intended use` block; " +
			"the migration to Go internal packages has retired most user-facing crate APIs.)*\n\n")
		return
	}
	buf.WriteString("Per-crate intended-use blocks from `src/lib.rs`. The Rust convention " +
		"(CONVENTIONS.md §\"Intended use\") opens every agent-invocable crate with " +
		"this four-field block.\n\n")
	for _, c := range crates {
		fmt.Fprintf(buf, "### `%s` — `%s`\n\n", c.Name, c.Path)
		buf.WriteString(c.IntendedUse)
		buf.WriteString("\n\n")
	}
}

// ───────────────────────────── Skills ────────────────────────────────

type skillEntry struct {
	Name        string
	Description string
	Path        string
}

func loadSkills(root string) ([]skillEntry, error) {
	dir := filepath.Join(root, "skills")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	var out []skillEntry
	for _, path := range files {
		var raw struct {
			Skill struct {
				Name        string `toml:"name"`
				Description string `toml:"description"`
			} `toml:"skill"`
		}
		if _, err := toml.DecodeFile(path, &raw); err != nil {
			// Tolerate variant shapes — fall back to filename so a
			// malformed skill TOML still surfaces in CODEMAP.
			raw.Skill.Name = strings.TrimSuffix(filepath.Base(path), ".toml")
		}
		name := raw.Skill.Name
		if name == "" {
			name = strings.TrimSuffix(filepath.Base(path), ".toml")
		}
		out = append(out, skillEntry{
			Name:        name,
			Description: firstSentence(raw.Skill.Description),
			Path:        relPath(root, path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func writeSkills(buf *bytes.Buffer, skills []skillEntry) {
	buf.WriteString("## Skills (`skills/`)\n\n")
	if len(skills) == 0 {
		buf.WriteString("*(none)*\n\n")
		return
	}
	for _, s := range skills {
		fmt.Fprintf(buf, "- `%s`", s.Name)
		if s.Description != "" {
			fmt.Fprintf(buf, " — %s", s.Description)
		}
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

// ─────────────────────────── Scripts ─────────────────────────────────

type scriptEntry struct {
	Name        string
	Description string
}

func loadScripts(root string) ([]scriptEntry, error) {
	dir := filepath.Join(root, "scripts")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []scriptEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		full := filepath.Join(dir, e.Name())
		first, err := firstCommentLine(full, "#")
		if err != nil {
			return nil, err
		}
		// Strip a leading "scripts/<name> — " prefix if present so the
		// description doesn't redundantly repeat the filename.
		first = stripScriptPrefix(first, e.Name())
		out = append(out, scriptEntry{
			Name:        e.Name(),
			Description: first,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// stripScriptPrefix removes a "<filename> — " or "<filename>: " prefix
// from the front of `s` so script descriptions don't redundantly repeat
// their own filename. Matches both em-dash and ASCII hyphen separators.
func stripScriptPrefix(s, filename string) string {
	for _, sep := range []string{" — ", " - ", ": "} {
		prefix := filename + sep
		if strings.HasPrefix(s, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
		// Also try stripping a leading "scripts/" path-style prefix.
		prefix = "scripts/" + filename + sep
		if strings.HasPrefix(s, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(s, prefix))
		}
	}
	return s
}

func writeScripts(buf *bytes.Buffer, scripts []scriptEntry) {
	buf.WriteString("## Scripts (`scripts/`)\n\n")
	if len(scripts) == 0 {
		buf.WriteString("*(none)*\n\n")
		return
	}
	for _, s := range scripts {
		fmt.Fprintf(buf, "- `%s`", s.Name)
		if s.Description != "" {
			fmt.Fprintf(buf, " — %s", s.Description)
		}
		buf.WriteString("\n")
	}
	buf.WriteString("\n")
}

// ─────────────────────────── helpers ─────────────────────────────────

// firstCommentLine returns the first non-shebang, non-blank comment
// line of `path`. Comment marker (`#` or `//`) is stripped. Returns ""
// if the file is missing or has no leading comment.
func firstCommentLine(path, marker string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#!") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, marker) {
			return "", nil // hit code without a comment
		}
		content := strings.TrimPrefix(trimmed, marker)
		content = strings.TrimSpace(content)
		return content, nil
	}
	return "", scanner.Err()
}

// leadingCommentDescription returns the first non-shebang, non-blank line
// of leading comment text (joined across contiguous comment lines until
// the first blank line). marker is "#" for shell/toml, "//" for go/rust.
func leadingCommentDescription(path, marker string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var collected []string
	started := false
	for scanner.Scan() {
		line := scanner.Text()
		// Skip shebang.
		if !started && strings.HasPrefix(line, "#!") {
			continue
		}
		trimmed := strings.TrimSpace(line)
		// First blank line ends the leading-comment block.
		if started && trimmed == "" {
			break
		}
		// Stop if we leave the comment block (e.g. hit code).
		if trimmed != "" && !strings.HasPrefix(trimmed, marker) {
			break
		}
		if strings.HasPrefix(trimmed, marker) {
			content := strings.TrimPrefix(trimmed, marker)
			content = strings.TrimPrefix(content, " ")
			collected = append(collected, content)
			started = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return strings.Join(collected, " "), nil
}

// firstSentence trims `s` to its first sentence — a terminator (`.`,
// `!`, `?`) followed by whitespace or end-of-string. Terminators inside
// tokens like `.md`, `etc.)`, or `~/.claude` are skipped because the
// next character isn't whitespace, so the sentence continues past them.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '.' && c != '!' && c != '?' {
			continue
		}
		next := i + 1
		if next == len(s) {
			return s
		}
		switch s[next] {
		case ' ', '\t', '\n':
			return strings.TrimSpace(s[:next])
		}
	}
	return s
}

func relPath(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return rel
}

// Compile-time guards.
var _ = fs.WalkDir
