package construct

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"toolkit/internal/forge/registry"
)

// renderMarkdown produces the markdown body for a markdown- or dual-target
// schema. Convention:
//
//	---                                        (YAML frontmatter open)
//	date: 2026-05-15
//	slug: my-slug
//	title: My Title                            (when title field present)
//	kind: decision                             (when kind field present)
//	tags: [a, b]                               (when tags field present — inline YAML list)
//	---                                        (YAML frontmatter close)
//
//	## Section Heading
//
//	Single field value renders as a paragraph; blank lines separate
//	consecutive fields within a section.
//
//	## Another Section
//
//	- list-shaped field item one
//	- list-shaped field item two
//
// Frontmatter carries fields whose name is in the "metadata" allowlist
// (date, slug, title, kind, tags, project) plus any field declared with
// render_as="frontmatter". Section fields render inside their sections.
// Fields that match neither set are omitted from the output — keeps the
// document focused on what the schema explicitly declared as renderable.
func renderMarkdown(schema registry.Schema, fields map[string]FieldValue, extras map[string]string, slug, date string) string {
	var out strings.Builder

	// Frontmatter — write keys in a stable order: standard metadata
	// first, then any schema-declared frontmatter fields the standard
	// list didn't cover, then non-declared keys that round-tripped via
	// the `extras` channel (preserves `source:`, `supersedes:`,
	// `superseded_by:`, custom keys the schema doesn't model — chain
	// 617 T1 / gap #5). Extras come last so declared fields always win
	// on naming collisions.
	out.WriteString("---\n")
	written := make(map[string]bool)
	writeFM := func(name string) {
		if written[name] {
			return
		}
		v, ok := fields[name]
		if !ok || v.IsEmpty() {
			return
		}
		out.WriteString(fmt.Sprintf("%s: %s\n", name, v.AsJoined()))
		written[name] = true
	}
	// Always include date + slug (auto-stamped if absent from fields).
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	out.WriteString(fmt.Sprintf("date: %s\n", date))
	out.WriteString(fmt.Sprintf("slug: %s\n", slug))
	written["date"] = true
	written["slug"] = true
	for _, name := range []string{"title", "kind", "project", "scope"} {
		writeFM(name)
	}
	// Bug 1446: `tags` is declared optional_string in vault-note (the
	// historical shape — comma-string at the DB column for the surface
	// field on knowledge_pointer). Emit it in YAML inline-list form so
	// vault_read's frontmatter parser (which expects []string) round-trips
	// cleanly without the "cannot unmarshal !!str into []string" warning.
	// Downstream consumers (DB writes via AsJoined) are unaffected — the
	// comma-string semantics live in coerceFields + the DB column, not in
	// the on-disk frontmatter shape.
	if v, ok := fields["tags"]; ok && !v.IsEmpty() && !written["tags"] {
		items := tagsItems(v)
		if len(items) > 0 {
			out.WriteString(fmt.Sprintf("tags: [%s]\n", strings.Join(items, ", ")))
			written["tags"] = true
		}
	}
	for _, fd := range schema.Fields {
		if fd.RenderAs == "frontmatter" {
			writeFM(fd.Name)
		}
	}
	// Extras: emit non-declared frontmatter keys captured at parse time.
	// Sorted for stable output across rewrites. Skip any key already
	// written (declared fields with the same name take precedence).
	if len(extras) > 0 {
		extraKeys := make([]string, 0, len(extras))
		for k := range extras {
			if !written[k] {
				extraKeys = append(extraKeys, k)
			}
		}
		sort.Strings(extraKeys)
		for _, k := range extraKeys {
			v := extras[k]
			if v == "" {
				continue
			}
			out.WriteString(fmt.Sprintf("%s: %s\n", k, v))
			written[k] = true
		}
	}
	out.WriteString("---\n")

	// Resolve which [[sections]] to walk. For markdown- and dual-target
	// schemas, the storage block may carry its own Sections list; fall
	// back to the schema-level [[sections]].
	sections := schema.Sections
	storage := schema.ResolvedStorage()
	if storage.Markdown != nil && len(storage.Markdown.Sections) > 0 {
		sections = storage.Markdown.Sections
	}

	for _, section := range sections {
		out.WriteString("\n## ")
		out.WriteString(section.Heading)
		out.WriteString("\n\n")
		for _, name := range section.Fields {
			v, ok := fields[name]
			if !ok || v.IsEmpty() {
				continue
			}
			renderFieldValue(v, &out)
		}
		if section.StaticText != "" {
			out.WriteString(section.StaticText)
			if !strings.HasSuffix(section.StaticText, "\n") {
				out.WriteString("\n")
			}
		}
	}
	return out.String()
}

// tagsItems normalizes a FieldValue holding tags into a slice of
// individual tag strings. Handles both shapes coerceFields can produce
// for vault-note's optional_string tags field: a single comma-joined
// string ("a,b,c") or a List (if a future schema swaps the type). Empty
// items are dropped; surrounding whitespace is trimmed per item.
func tagsItems(v FieldValue) []string {
	var raw []string
	if v.IsList {
		raw = v.List
	} else {
		raw = strings.Split(v.Single, ",")
	}
	items := make([]string, 0, len(raw))
	for _, t := range raw {
		if t = strings.TrimSpace(t); t != "" {
			items = append(items, t)
		}
	}
	return items
}

// renderFieldValue writes one field's value in markdown shape. Single
// strings become a paragraph (value + blank line); list values become a
// bulleted list (each item + final blank line). Mirrors the Rust
// renderer's render_field_value with the table mode intentionally
// dropped — table rendering is YAGNI for vault entries and the Go port
// adds it back if a future schema declares render_as="table".
func renderFieldValue(v FieldValue, out *strings.Builder) {
	if v.IsList {
		for _, item := range v.List {
			out.WriteString("- ")
			out.WriteString(item)
			out.WriteString("\n")
		}
		out.WriteString("\n")
		return
	}
	out.WriteString(v.Single)
	out.WriteString("\n\n")
}

// buildOutputPath substitutes placeholders in the schema's
// filename_pattern and joins under output_dir. Both come from
// ResolvedStorage() so markdown-target and dual-target schemas share
// the helper. The returned path is relative to repo / vault root;
// callers supply that root explicitly.
//
// Supported placeholders in filename_pattern:
//
//	{prefix}    storage.Prefix
//	{slug}      slug argument
//	{date}      date argument (auto-stamped UTC if empty)
//	{kind}      fields["kind"].Single, when present
//	{subdir}    fields["subdir"].Single, when present — derived
//	            subdir for kind+scope-routed schemas (vault-note)
//	{project}   fields["project"].Single, when present
//	{scope}     fields["scope"].Single, when present — vault-note's
//	            routing input post-`forge-vault-note-schema-rework`
//
// Unknown placeholders pass through unchanged so a future schema can
// declare them without changing this code.
func buildOutputPath(root string, schema registry.Schema, slug, date string, fields map[string]FieldValue) string {
	storage := schema.ResolvedStorage()
	prefix := storage.Prefix
	outputDir := storage.OutputDir
	pattern := storage.FilenamePattern
	if storage.Markdown != nil {
		prefix = storage.Markdown.Prefix
		outputDir = storage.Markdown.OutputDir
		pattern = storage.Markdown.FilenamePattern
	}
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	name := pattern
	name = strings.ReplaceAll(name, "{prefix}", prefix)
	name = strings.ReplaceAll(name, "{slug}", slug)
	name = strings.ReplaceAll(name, "{date}", date)
	for _, ph := range []string{"kind", "subdir", "project", "scope", "chain_slug_upper"} {
		if v, ok := fields[ph]; ok && !v.IsList {
			name = strings.ReplaceAll(name, "{"+ph+"}", v.Single)
		}
	}
	return filepath.Join(root, outputDir, name)
}

// atomicWrite writes content to path via tempfile + rename. Creates
// parent directories. Refuses to write if path escapes the supplied
// rootGuard (covers the constraint "don't auto-create parent directories
// outside output_dir"). The rootGuard is the schema's output_dir
// resolved against root — callers pass the full prefix that the write
// must stay underneath.
func atomicWrite(path, rootGuard string, content []byte) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	absGuard, err := filepath.Abs(rootGuard)
	if err != nil {
		return fmt.Errorf("resolve guard: %w", err)
	}
	rel, err := filepath.Rel(absGuard, absPath)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to write %q outside output_dir %q", path, rootGuard)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(absPath), ".forge-write-*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op if rename succeeds
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
