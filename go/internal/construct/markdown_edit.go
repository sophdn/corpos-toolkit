package construct

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"toolkit/internal/db"
	"toolkit/internal/forge/registry"
)

// editMarkdownWithMerged is the body of editMarkdown plus the merged
// post-overlay field map as a second return. editMarkdown discards it
// (no event to build); EditMarkdownArtifact (the exported construct seam)
// keeps it.
func editMarkdownWithMerged(ctx context.Context, q db.Queryer, schema registry.Schema, project, slug string, partial map[string]FieldValue, opts EditOpts) (EditResult, map[string]FieldValue, error) {
	root := resolveMarkdownRoot(ctx, q, project, schema)
	storage := schema.ResolvedStorage()
	outputDir := storage.OutputDir
	pattern := storage.FilenamePattern
	if storage.Markdown != nil {
		if storage.Markdown.OutputDir != "" {
			outputDir = storage.Markdown.OutputDir
		}
		if storage.Markdown.FilenamePattern != "" {
			pattern = storage.Markdown.FilenamePattern
		}
	}
	guard := filepath.Join(root, outputDir)

	// Locate-pattern identity pinning, keyed on the DECLARATIVE filename_pattern
	// (no schemaName switch — chain refactor-forge-shape-dispatch T7):
	//
	//   - {slug}-keyed patterns (vault-note: {subdir}/{date}_{slug}.md) are
	//     pinned by findMarkdownPath on {slug}; the derived routing fields
	//     (e.g. {subdir}) deliberately stay globs so a relocated file is still
	//     found.
	//   - slug-LESS patterns (chain-anchored docs: docs/{chain_slug_upper}_
	//     RETROSPECTIVE_{date}.md — identity is chain_slug, not slug) would
	//     otherwise match every doc in the dir → ambiguous-slug. Pin the
	//     shape's derived routing fields (from the caller-supplied input via the
	//     same Strategy.DeriveRoutingFields the rewrite uses) so the search
	//     resolves THIS artifact. A future slug-less shape gets this for free.
	//     Bug forge-edit-on-retrospective-report-card-likely-relocates-editmarkdown-doesn-t.
	locatePattern := pattern
	if !strings.Contains(pattern, "{slug}") {
		extra, _, _ := deriveRoutingFields(schema.Meta.Name, project, slug, partial)
		for k, v := range extra {
			locatePattern = strings.ReplaceAll(locatePattern, "{"+k+"}", v.AsJoined())
		}
	}

	path, err := findMarkdownPath(guard, locatePattern, slug)
	if err != nil {
		if os.IsNotExist(err) {
			return EditResult{NotFound: true}, nil, nil
		}
		return EditResult{}, nil, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EditResult{NotFound: true}, nil, nil
		}
		return EditResult{}, nil, fmt.Errorf("read %s: %w", path, err)
	}

	existing, extras, body, err := parseMarkdownDoc(string(raw), schema)
	if err != nil {
		return EditResult{}, nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}

	// Apply opts.DropExtras: remove caller-named non-declared frontmatter
	// keys from the round-trip channel BEFORE merge so they don't land in
	// the rewrite. A name on the request that isn't present on the source
	// is a no-op (idempotent drop). dropped lists what actually shrank;
	// fed back via EditResult.DroppedExtras so the caller can verify the
	// targeted keys were on the source in the first place.
	var dropped []string
	if len(opts.DropExtras) > 0 {
		for _, k := range opts.DropExtras {
			if _, present := extras[k]; present {
				delete(extras, k)
				dropped = append(dropped, k)
			}
		}
	}

	// Merge: start from existing, override per caller's partial fields.
	// Tracking which fields the caller actually touched separates "we
	// preserved this on rewrite" from "the caller edited this field".
	// Non-declared frontmatter keys (`extras`) are NOT in the partial
	// payload — they round-trip via a separate channel through
	// renderMarkdown to preserve `source:`, `supersedes:`,
	// `superseded_by:`, etc. on edit (chain 617 T1 spot-check / gap #5).
	merged := make(map[string]FieldValue, len(existing)+len(partial))
	for k, v := range existing {
		merged[k] = v
	}
	var updated []string
	for k, v := range partial {
		merged[k] = v
		updated = append(updated, k)
	}

	// Re-derive the filename-routing fields createMarkdown injects, from the
	// POST-merge state (the caller may have changed note_kind, which drives
	// vault-note's {subdir}). This is the SAME derivation the create path runs
	// — owned once by the shape's Strategy.DeriveRoutingFields (chain
	// refactor-forge-shape-dispatch T6c collapses the create↔edit routing-field
	// duplication; T3 Axis 1). vault-note → {subdir} + a routingNote;
	// retro/report-card → {chain_slug_upper} (filename-only, never frontmatter,
	// so parse never recovers it; re-derived from the preserved chain_slug so
	// the rewrite matches the located file instead of relocating to a literal
	// "{chain_slug_upper}" name).
	extra, routingNote, _ := deriveRoutingFields(schema.Meta.Name, project, slug, merged)
	mergeDerived(merged, extra)

	// renderMarkdown wants explicit slug + date — pass through what we
	// parsed (or auto-stamp date if absent from existing frontmatter).
	date := stringField(merged, "date")
	if date == "" {
		date = currentDate()
		merged["date"] = SingleValue(date)
	}
	// Suppress unused-import lint when body never read elsewhere; the
	// parser returns body for future-use (e.g. recovering content past
	// the section list). renderMarkdown rebuilds the body from fields.
	_ = body

	rewritten := renderMarkdown(schema, merged, extras, slug, date)

	// Compute the target path from post-merge routing fields. If the
	// schema's filename_pattern routes on a field the caller changed
	// (vault-note's note_kind drives {subdir}), this differs from the
	// current location and the edit needs to move the file. atomicWrite
	// enforces that newPath stays under the rootGuard; if it escapes
	// (e.g. a malformed kind that yields a "../" subdir), the write is
	// rejected and the original file stays untouched.
	newPath := buildOutputPath(root, schema, slug, date, merged)
	if filepath.Clean(newPath) == filepath.Clean(path) {
		if err := atomicWrite(path, guard, []byte(rewritten)); err != nil {
			return EditResult{}, nil, err
		}
		return EditResult{
			UpdatedFields: updated,
			ArtifactPath:  path,
			Action:        "updated",
			RoutingNote:   routingNote,
			DroppedExtras: dropped,
		}, merged, nil
	}
	if err := atomicWrite(newPath, guard, []byte(rewritten)); err != nil {
		return EditResult{}, nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// New file landed but the old one survives — surface the partial
		// state with both paths so the caller can clean up. findMarkdownPath
		// would otherwise return "ambiguous slug" on subsequent reads.
		return EditResult{}, nil, fmt.Errorf("forge_edit: wrote %s but failed to remove stale %s: %w", newPath, path, err)
	}
	return EditResult{
		UpdatedFields: updated,
		ArtifactPath:  newPath,
		Relocated:     true,
		Action:        "updated",
		RoutingNote:   routingNote,
		DroppedExtras: dropped,
	}, merged, nil
}

// findMarkdownPath locates the markdown file for a given slug by globbing
// schema.filename_pattern under rootDir. Returns os.ErrNotExist when no
// file matches; an error when multiple match (ambiguous slug); the
// absolute path otherwise. Walks the tree because patterns may include
// placeholders with embedded slashes (vault-note's {subdir} resolves to
// e.g. "learnings/mcp-servers", spanning two directory levels).
//
// Two-pass match: first against the canonical pattern (e.g.
// `{subdir}/{date}_{slug}.md`); if zero matches AND the pattern carries
// a `{date}_` segment, retries against the same pattern with that
// segment stripped (e.g. `{subdir}/{slug}.md`). This locates
// pre-canonical entries whose filenames lack the date prefix (chain 617
// T1 spot-check / gap #3: `reference/research-backlog.md`,
// `decisions/mcp-architecture-design.md`, etc.). The relocation logic
// in editMarkdown will move them to the canonical dated path on the
// rewrite, so the fallback is read-only — it doesn't change the
// canonical authoring shape.
func findMarkdownPath(rootDir, filenamePattern, slug string) (string, error) {
	if rootDir == "" || filenamePattern == "" {
		return "", fmt.Errorf("findMarkdownPath: rootDir and filenamePattern required")
	}
	if _, err := os.Stat(rootDir); err != nil {
		if os.IsNotExist(err) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	walk := func(re *regexp.Regexp) ([]string, error) {
		var matches []string
		walkErr := filepath.WalkDir(rootDir, func(p string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(rootDir, p)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if re.MatchString(rel) {
				matches = append(matches, p)
			}
			return nil
		})
		return matches, walkErr
	}

	primary, err := patternToSearchRegex(filenamePattern, slug)
	if err != nil {
		return "", err
	}
	matches, walkErr := walk(primary)
	if walkErr != nil {
		return "", walkErr
	}
	if len(matches) == 0 && strings.Contains(filenamePattern, "{date}_") {
		// Fallback: try the pattern without the `{date}_` prefix. Pre-
		// canonical entries (no date prefix in filename) only match this
		// shape; editMarkdown's relocation path handles the rename to
		// the dated form on rewrite.
		fallbackPattern := strings.ReplaceAll(filenamePattern, "{date}_", "")
		fallback, err := patternToSearchRegex(fallbackPattern, slug)
		if err != nil {
			return "", err
		}
		matches, walkErr = walk(fallback)
		if walkErr != nil {
			return "", walkErr
		}
	}
	switch len(matches) {
	case 0:
		return "", os.ErrNotExist
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous slug %q: %d files match (%s)", slug, len(matches), strings.Join(matches, ", "))
	}
}

// patternToSearchRegex turns a filename_pattern like
// "{subdir}/{date}_{slug}.md" into a regex anchored to the basename
// search: literal characters are escaped, {slug} is pinned to the
// caller's slug, every other placeholder collapses to ".+" so directory
// levels and date strings are accepted.
func patternToSearchRegex(pattern, slug string) (*regexp.Regexp, error) {
	placeholderRE := regexp.MustCompile(`\{[a-z_]+\}`)
	literals := placeholderRE.Split(pattern, -1)
	placeholders := placeholderRE.FindAllString(pattern, -1)
	var b strings.Builder
	b.WriteString("^")
	for i, lit := range literals {
		b.WriteString(regexp.QuoteMeta(lit))
		if i < len(placeholders) {
			if placeholders[i] == "{slug}" {
				b.WriteString(regexp.QuoteMeta(slug))
			} else {
				b.WriteString(`.+`)
			}
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// parseMarkdownDoc inverts renderMarkdown for the conventional layout it
// emits: YAML frontmatter between leading `---` lines, then `## Heading`
// sections each followed by the field's value as a paragraph or
// bulleted list. Returns the recovered field map and the body remainder
// (callers may use the body for diff purposes; today it's unused but
// kept so the parser stays general).
//
// Limitations:
//   - section bodies that themselves contain "\n## " headings will be
//     misread as new sections. Vault notes don't typically embed level-2
//     headings inside body fields; cross that bridge if a schema needs it.
//   - multi-field sections retain the value verbatim into the first
//     declared field. Schemas with multi-field sections aren't in the
//     current vault-note shape; revisit when one ships.
func parseMarkdownDoc(content string, schema registry.Schema) (map[string]FieldValue, map[string]string, string, error) {
	fields := map[string]FieldValue{}
	extras := map[string]string{}
	fmMap, body := splitDocFrontmatter(content)
	declared := make(map[string]registry.Field, len(schema.Fields))
	for _, f := range schema.Fields {
		declared[f.Name] = f
	}

	for k, v := range fmMap {
		if fd, ok := declared[k]; ok {
			fields[k] = frontmatterToFieldValue(v, fd)
			continue
		}
		// Synthesised meta keys (date, slug, subdir) are not schema
		// fields but are read into the merge map so re-render can
		// recover them.
		if k == "date" || k == "slug" || k == "subdir" {
			fields[k] = SingleValue(v)
			continue
		}
		// `created:` is the pre-canonical alias for `date:` (drift
		// catalog A1). It's handled explicitly below — don't echo it
		// back through extras or both `created:` and `date:` would
		// land in the rewrite.
		if k == "created" {
			continue
		}
		// Non-declared frontmatter keys (`source:`, `supersedes:`,
		// `superseded_by:`, custom shapes the schema doesn't model)
		// round-trip via this channel so forge_edit stops dropping them.
		// Chain 617 T1 spot-check / gap #5: the SKILL's "custom
		// frontmatter shapes" exception was the only way to preserve
		// these; making forge round-trip them lets forge stay canonical
		// for the full vault corpus.
		extras[k] = v
	}

	sections := schema.Sections
	storage := schema.ResolvedStorage()
	if storage.Markdown != nil && len(storage.Markdown.Sections) > 0 {
		sections = storage.Markdown.Sections
	}
	declaredHeadings := make(map[string]bool, len(sections))
	for _, s := range sections {
		declaredHeadings[s.Heading] = true
	}
	sectionBodies := splitDocSections(body, declaredHeadings)
	matchedAny := false
	for _, section := range sections {
		text, ok := sectionBodies[section.Heading]
		if !ok || len(section.Fields) == 0 {
			continue
		}
		matchedAny = true
		fieldName := section.Fields[0]
		fd, ok := declared[fieldName]
		if !ok {
			continue
		}
		fields[fieldName] = bodyToFieldValue(text, fd)
	}

	// Body recovery for non-canonical files (chain 617 T1 / bug
	// `forge-edit-markdown-wipes-noncanonical-body-and-resets-date`):
	// when the document has zero recognised schema-section headings but
	// post-frontmatter body content is present, attribute that body to
	// the last declared section's first field. Pre-canonical vault
	// entries (authored via Write, or by older forge versions before
	// createMarkdown emitted `## Body`) lack the `## <Heading>` markers
	// the loop above relies on; without this fallback their content is
	// silently dropped on rewrite. Conservative trigger (zero matches,
	// not partial) so a canonical file missing one section doesn't get
	// its remaining body misattributed.
	if !matchedAny && strings.TrimSpace(body) != "" && len(sections) > 0 {
		last := sections[len(sections)-1]
		if len(last.Fields) > 0 {
			fieldName := last.Fields[0]
			if _, alreadySet := fields[fieldName]; !alreadySet {
				if fd, ok := declared[fieldName]; ok {
					fields[fieldName] = bodyToFieldValue(body, fd)
				}
			}
		}
	}

	// `created:` is the pre-canonical alias for `date:` (drift catalog
	// A1). When the existing file has `created:` but no `date:`, migrate
	// the value into the date synth key so re-render emits `date:` and
	// the file's original date is preserved across the rewrite. Without
	// this, editMarkdown's date-defaulting falls back to currentDate()
	// and the file relocates to today (chain 617 T1 / data-loss bug).
	if _, hasDate := fields["date"]; !hasDate {
		if created, ok := fmMap["created"]; ok && created != "" {
			fields["date"] = SingleValue(created)
		}
	}
	return fields, extras, body, nil
}

// splitDocFrontmatter extracts the leading `---`-delimited YAML
// frontmatter as a key→value map (single-line scalar values only — the
// renderer never emits multi-line YAML). Returns nil + the original
// content when no frontmatter is present.
//
// Inline YAML list values like `tags: [a, b, c]` (bug 1446 fix shape)
// are normalized to a comma-joined string ("a,b,c") so downstream
// frontmatterToFieldValue stays a single straight string→FieldValue
// coercion without per-key shape branching. The bracket-strip is
// keyed on the literal `[...]` envelope; legitimate string values that
// happen to start with `[` are not in the vault-note vocabulary.
func splitDocFrontmatter(content string) (map[string]string, string) {
	const open = "---\n"
	if !strings.HasPrefix(content, open) {
		return nil, content
	}
	rest := content[len(open):]
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		// Tolerate trailing "\n---" with no following newline.
		if strings.HasSuffix(rest, "\n---") {
			closeIdx = len(rest) - len("\n---")
		} else {
			return nil, content
		}
	}
	fmText := rest[:closeIdx]
	bodyStart := closeIdx + len("\n---\n")
	if bodyStart > len(rest) {
		bodyStart = len(rest)
	}
	body := rest[bodyStart:]
	out := map[string]string{}
	for _, line := range strings.Split(fmText, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]") {
			inner := strings.TrimSpace(val[1 : len(val)-1])
			if inner == "" {
				val = ""
			} else {
				parts := strings.Split(inner, ",")
				for i, p := range parts {
					parts[i] = strings.TrimSpace(p)
				}
				val = strings.Join(parts, ",")
			}
		}
		out[key] = val
	}
	return out, body
}

// splitDocSections walks the body for "\n## <heading>" markers (or one
// at offset 0) and maps each schema-declared heading to the trimmed
// content that runs up to the NEXT schema-declared heading. The
// `declared` argument filters which `## ` markers count as section
// boundaries; in-body H2 headings that aren't in the declared set
// (e.g. `## Context`, `## Decision` inside a Body section's prose) are
// treated as content, not section starts. Without this filter, any
// embedded H2 in a section's body would truncate that section at the
// first embedded marker — silently dropping content past it on the
// next forge_edit (chain 617 spot-check / bug
// `forge-edit-splitdocsections-treats-embedded-h2-as-section-boundary`).
//
// The renderer emits a blank line before each declared `## ` heading
// so the regex anchors on start-of-line. Passing an empty/nil declared
// map preserves the prior "treat every `## ` as a section" behavior
// for any caller that wants raw section discovery.
func splitDocSections(body string, declared map[string]bool) map[string]string {
	re := regexp.MustCompile(`(?m)^## (.+)$`)
	matches := re.FindAllStringSubmatchIndex(body, -1)
	type filteredMatch struct {
		start, end int
		heading    string
	}
	var filtered []filteredMatch
	for _, m := range matches {
		heading := body[m[2]:m[3]]
		if len(declared) > 0 && !declared[heading] {
			continue
		}
		filtered = append(filtered, filteredMatch{start: m[0], end: m[1], heading: heading})
	}
	out := map[string]string{}
	for i, m := range filtered {
		var sectionEnd int
		if i+1 < len(filtered) {
			sectionEnd = filtered[i+1].start
		} else {
			sectionEnd = len(body)
		}
		content := strings.TrimSpace(body[m.end:sectionEnd])
		out[m.heading] = content
	}
	return out
}

// frontmatterToFieldValue coerces a frontmatter scalar back into a
// FieldValue. For *_list types the value was comma-joined by
// FieldValue.AsJoined; split it. For everything else the value is a
// single string.
func frontmatterToFieldValue(raw string, fd registry.Field) FieldValue {
	if fd.Type.IsList() {
		var items []string
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				items = append(items, p)
			}
		}
		return ListValue(items)
	}
	return SingleValue(raw)
}

// bodyToFieldValue interprets a section body. List-shaped fields parse
// "- item" lines; scalars take the body as a single value. Whitespace
// is trimmed at the ends; internal blank lines are preserved so
// multi-paragraph content round-trips through edit/re-render.
func bodyToFieldValue(text string, fd registry.Field) FieldValue {
	if fd.Type.IsList() {
		var items []string
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "- ") {
				items = append(items, strings.TrimSpace(line[2:]))
			}
		}
		return ListValue(items)
	}
	return SingleValue(strings.TrimSpace(text))
}

// EditMarkdownArtifact is the exported seam over the generic editMarkdown
// path — wraps it so the construct edit path (Stage 3 Slice 4: retrospective
// + report-card) can read the post-merge fields map. Returns the EditResult
// + the merged field map (existing parsed from disk + caller's partial
// overlay).
//
// Same semantics as editMarkdown (which forge_edit on vault-note / retro /
// report-card already runs): the generic markdown rewrite path, no
// chain-anchor resolve, no skeleton-section count. construct's per-schema
// buildEdit* then layers those concerns on top (chain anchor resolve via
// the exported seam, section count via the exported skeleton field lists).
func EditMarkdownArtifact(ctx context.Context, q db.Queryer, schema registry.Schema, project, slug string, partial map[string]FieldValue, opts EditOpts) (EditResult, map[string]FieldValue, error) {
	res, merged, err := editMarkdownWithMerged(ctx, q, schema, project, slug, partial, opts)
	return res, merged, err
}

// EditMemoryArtifact is the dedicated edit path for the memory schema —
// exported as a forge seam (Stage 3 of chain 311 T7) so the construct edit
// path can re-home its file write through forge's renderer + parser
// without re-implementing them.
//
// The generic editMarkdown can't round-trip memory notes: memory's
// frontmatter is the auto-load shape (top-level name/description plus a
// nested metadata: block — splitDocFrontmatter flattens that to bogus
// top-level keys), and its {kind} filename token resolves from
// memory_kind, not the `kind` field buildOutputPath reads. createMarkdown
// special-cases both (renderMemoryMarkdown + a memory_kind→kind alias);
// this mirrors them on edit. Without it, forge_edit relocated memory
// notes into a literal "{kind}/" directory and rewrote their frontmatter
// into the flat generic shape, dropping `description`. Bug
// forge-edit-on-the-memory-schema-corrupts-notes-literal-kind-dir-relocation.
//
// Returns the EditResult (path, relocated flag, routing note, updated-field
// names) AND the post-merge fields map (existing parsed from disk + the
// caller's partial overlay). The merged map lets construct.Update build a
// MemoryWritten event reflecting the final on-disk state.
func EditMemoryArtifact(ctx context.Context, q db.Queryer, schema registry.Schema, project, slug string, partial map[string]FieldValue) (EditResult, map[string]FieldValue, error) {
	root := resolveMarkdownRoot(ctx, q, project, schema)
	storage := schema.ResolvedStorage()
	outputDir := storage.OutputDir
	pattern := storage.FilenamePattern
	if storage.Markdown != nil {
		if storage.Markdown.OutputDir != "" {
			outputDir = storage.Markdown.OutputDir
		}
		if storage.Markdown.FilenamePattern != "" {
			pattern = storage.Markdown.FilenamePattern
		}
	}
	guard := filepath.Join(root, outputDir)

	path, err := findMarkdownPath(guard, pattern, slug)
	if err != nil {
		if os.IsNotExist(err) {
			return EditResult{NotFound: true}, nil, nil
		}
		return EditResult{}, nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return EditResult{NotFound: true}, nil, nil
		}
		return EditResult{}, nil, fmt.Errorf("read %s: %w", path, err)
	}

	existing, existingProject := parseMemoryDoc(string(raw))

	// Merge: existing values first, caller's partial wins.
	merged := make(map[string]FieldValue, len(existing)+len(partial))
	for k, v := range existing {
		merged[k] = v
	}
	var updated []string
	for k, v := range partial {
		merged[k] = v
		updated = append(updated, k)
	}

	// Effective project: the dispatch scope, falling back to the project
	// recorded in the existing metadata block so a cross-project edit
	// (empty dispatch project) preserves it on rewrite.
	effProject := project
	if effProject == "" {
		effProject = existingProject
	}

	rewritten := renderMemoryMarkdown(effProject, slug, merged)

	// Path routing: {kind} resolves from memory_kind. Alias it so
	// buildOutputPath yields memory/<kind>/<slug>.md (mirrors create).
	// A caller-changed memory_kind is a legitimate relocation; otherwise
	// the path is unchanged and we rewrite in place.
	memKind := stringField(merged, "memory_kind")
	merged["kind"] = SingleValue(memKind)
	newPath := buildOutputPath(root, schema, slug, stringField(merged, "date"), merged)
	routingNote := fmt.Sprintf("routed to memory/%s (memory_kind=%q)", memKind, memKind)

	if filepath.Clean(newPath) == filepath.Clean(path) {
		if err := atomicWrite(path, guard, []byte(rewritten)); err != nil {
			return EditResult{}, nil, err
		}
		return EditResult{
			UpdatedFields: updated,
			ArtifactPath:  path,
			Action:        "updated",
			RoutingNote:   routingNote,
		}, merged, nil
	}
	if err := atomicWrite(newPath, guard, []byte(rewritten)); err != nil {
		return EditResult{}, nil, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return EditResult{}, nil, fmt.Errorf("forge_edit(memory): wrote %s but failed to remove stale %s: %w", newPath, path, err)
	}
	return EditResult{
		UpdatedFields: updated,
		ArtifactPath:  newPath,
		Relocated:     true,
		Action:        "updated",
		RoutingNote:   routingNote,
	}, merged, nil
}

// parseMemoryDoc recovers the memory-note fields from a rendered memory
// markdown document — the inverse of renderMemoryMarkdown. Returns the
// field map (memory_kind, description, source, observed_first,
// recurrence_count, body) plus the project recorded in the metadata
// block (carried separately because renderMemoryMarkdown takes project
// as a positional argument, not a field). The `name:` line is ignored —
// the slug is the authoritative identity and is supplied by the caller.
// Tolerant of missing optional keys and of a body with no frontmatter.
func parseMemoryDoc(content string) (map[string]FieldValue, string) {
	fields := map[string]FieldValue{}
	var project string

	const open = "---\n"
	if !strings.HasPrefix(content, open) {
		fields["body"] = SingleValue(strings.TrimLeft(content, "\n"))
		return fields, project
	}
	rest := content[len(open):]
	closeIdx := strings.Index(rest, "\n---\n")
	if closeIdx < 0 {
		fields["body"] = SingleValue(rest)
		return fields, project
	}
	fmText := rest[:closeIdx]
	body := rest[closeIdx+len("\n---\n"):]
	fields["body"] = SingleValue(strings.TrimLeft(body, "\n"))

	inMeta := false
	for _, line := range strings.Split(fmText, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indented := strings.HasPrefix(line, "  ")
		trimmed := strings.TrimSpace(line)
		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		val := strings.TrimSpace(trimmed[idx+1:])
		if !indented {
			// Top-level key ends any metadata block we were inside.
			inMeta = key == "metadata"
			if key == "description" {
				fields["description"] = SingleValue(val)
			}
			continue
		}
		if !inMeta {
			continue
		}
		switch key {
		case "type":
			fields["memory_kind"] = SingleValue(val)
		case "project":
			project = val
		case "source":
			fields["source"] = SingleValue(val)
		case "observed_first":
			fields["observed_first"] = SingleValue(val)
		case "recurrence_count":
			fields["recurrence_count"] = SingleValue(val)
		}
	}
	return fields, project
}

// EditOpts carries edit-only modifiers consumed by the markdown rewrite path
// (relocated from forge/edit.go for the P2-C.2 archive). DropExtras names
// non-declared frontmatter keys to remove from a markdown artifact's
// preserved-extras channel on rewrite; the default round-trips every
// non-declared key the source carried. Names absent from the source are no-ops.
type EditOpts struct {
	DropExtras []string
}
