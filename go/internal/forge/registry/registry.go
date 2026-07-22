// Package registry loads forge schema TOML files and exposes a lookup API.
//
// A schema describes one forgeable artifact type: its identity (name,
// prefix, output_dir, filename_pattern), its storage target (db, markdown,
// or dual), the fields a forge call must supply, and the rendered section
// layout. Per-project blueprint directories register additional schemas on
// top of the core directory shipped with mcp-servers.
//
// Schema-name uniqueness is enforced across registered directories: the
// first registration wins, the second is reported as a SchemaConflict.
// Hot reload is supported via Reload(); admin.schema_reload depends on
// this so adding a new schema TOML does not require a server restart.
package registry

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"toolkit/internal/obs"
)

// FieldType is the declared shape of a field value.
type FieldType string

const (
	FieldTypeString               FieldType = "string"
	FieldTypeStringList           FieldType = "string_list"
	FieldTypeOptionalString       FieldType = "optional_string"
	FieldTypeOptionalStringList   FieldType = "optional_string_list"
	FieldTypeStringOrList         FieldType = "string_or_list"
	FieldTypeOptionalStringOrList FieldType = "optional_string_or_list"
)

// IsRequired reports whether a field of this type must be present and
// non-empty at create time. Mirrors the Rust FieldDefinition::is_required
// rule.
func (t FieldType) IsRequired() bool {
	switch t {
	case FieldTypeString, FieldTypeStringList, FieldTypeStringOrList:
		return true
	}
	return false
}

// IsList reports whether this type produces a list value (single-string
// inputs to *_or_list types are coerced at validate time).
func (t FieldType) IsList() bool {
	switch t {
	case FieldTypeStringList, FieldTypeOptionalStringList,
		FieldTypeStringOrList, FieldTypeOptionalStringOrList:
		return true
	}
	return false
}

func (t FieldType) known() bool {
	switch t {
	case FieldTypeString, FieldTypeStringList, FieldTypeOptionalString,
		FieldTypeOptionalStringList, FieldTypeStringOrList, FieldTypeOptionalStringOrList:
		return true
	}
	return false
}

// ForeignKey is the declarative reference from one shape's field to another
// shape's column. Validation is a pre-op hook in the forge engine, not a
// schema-load-time check.
type ForeignKey struct {
	Shape  string `toml:"shape"`
	Column string `toml:"column"`
}

// Field is one declared field on a schema.
type Field struct {
	Name         string      `toml:"name"`
	Type         FieldType   `toml:"type"`
	Description  string      `toml:"description"`
	RenderAs     string      `toml:"render_as,omitempty"`
	TableColumns []string    `toml:"table_columns,omitempty"`
	EnumValues   []string    `toml:"enum_values,omitempty"`
	Pattern      string      `toml:"pattern,omitempty"`
	ForeignKey   *ForeignKey `toml:"foreign_key,omitempty"`
	Requires     []string    `toml:"requires,omitempty"`
	// SetBy names a dedicated lifecycle action that OWNS this field's
	// value — the field is NOT directly editable via forge_edit. When
	// set, forge_edit rejects an attempt to update the field pre-emit,
	// naming the owning action. Used for fields that live on an event
	// payload rather than the projection (e.g. bug.resolution_note rides
	// on the BugResolved event, set by bug_resolve; the projection
	// dropped the column in migration 065). Without this annotation
	// forge_edit emits an edit event whose fold crashes with "unknown
	// column". Bug `resolution-note-machinery-incomplete-on-bug-resolve-
	// and-forge-edit`.
	SetBy string `toml:"set_by,omitempty"`
}

// Section is one rendered markdown section. `Fields` enumerates the field
// names whose values fill the section; `StaticText` is boilerplate appended
// after the field values.
type Section struct {
	Heading    string   `toml:"heading"`
	Fields     []string `toml:"fields,omitempty"`
	StaticText string   `toml:"static_text,omitempty"`
}

// HookSpec is a declarative hook reference. The named callback must be
// registered with the forge engine at startup; unknown IDs are reported
// at load time once T50 wires the registry.
type HookSpec struct {
	Event     string `toml:"event"`
	Callback  string `toml:"callback"`
	HookClass string `toml:"hook_class"`
}

// StorageTarget enumerates the persistence model for a schema.
type StorageTarget string

const (
	StorageTargetMarkdown StorageTarget = "markdown"
	StorageTargetDB       StorageTarget = "db"
	StorageTargetDual     StorageTarget = "dual"
)

// MarkdownStorage describes the on-disk file output for a schema.
type MarkdownStorage struct {
	Prefix          string    `toml:"prefix"`
	OutputDir       string    `toml:"output_dir"`
	FilenamePattern string    `toml:"filename_pattern"`
	Sections        []Section `toml:"sections,omitempty"`
}

// DBStorage describes the operational SQLite row output for a schema.
type DBStorage struct {
	Table      string            `toml:"table"`
	KeyColumns []string          `toml:"key_columns"`
	ScopeParam string            `toml:"scope_param,omitempty"`
	ColumnMap  map[string]string `toml:"column_map,omitempty"`
}

// Storage is the TOML [storage] block. The TOML form is asymmetric:
// `target = "markdown"` flattens MarkdownStorage fields under [storage];
// `target = "db"` flattens DBStorage fields under [storage]; `target =
// "dual"` nests [storage.markdown] + [storage.db]. The parser handles
// each form. Use Resolved() to obtain the normalized representation.
type Storage struct {
	Target StorageTarget `toml:"target"`
	// Tuple-variant fields (flat under [storage] for markdown/db targets).
	Prefix          string            `toml:"prefix,omitempty"`
	OutputDir       string            `toml:"output_dir,omitempty"`
	FilenamePattern string            `toml:"filename_pattern,omitempty"`
	Table           string            `toml:"table,omitempty"`
	KeyColumns      []string          `toml:"key_columns,omitempty"`
	ScopeParam      string            `toml:"scope_param,omitempty"`
	ColumnMap       map[string]string `toml:"column_map,omitempty"`
	// Struct-variant fields (nested for dual target).
	Markdown *MarkdownStorage `toml:"markdown,omitempty"`
	DB       *DBStorage       `toml:"db,omitempty"`
}

// Metadata is the [schema] block within a schema TOML.
//
// CrossProject marks a schema as cross-project: it exempts the schema
// from the forge dispatcher's project-required gate (top-level
// `project` is optional, not required) and signals that the dispatcher
// should not auto-inject the top-level project into a same-named
// schema field. vault-note is the canonical cross-project schema (see
// chain `forge-vault-note-schema-rework` closure for the rename that
// introduced this flag in place of the prior "schema declares a field
// named project" heuristic).
type Metadata struct {
	Name            string `toml:"name"`
	Prefix          string `toml:"prefix"`
	OutputDir       string `toml:"output_dir"`
	FilenamePattern string `toml:"filename_pattern"`
	CrossProject    bool   `toml:"cross_project,omitempty"`
}

// Lifecycle is the [lifecycle] block within a schema TOML. Carries
// soft-cancel-action declarations for schemas whose state transitions
// are owned by dedicated MCP actions rather than forge_delete (chain ↔
// chain_close, task ↔ task_cancel, bug ↔ bug_resolve). When set, the
// forge_delete handler surfaces the named action in its rejection
// envelope so callers don't have to re-discover it from the action list.
type Lifecycle struct {
	SoftDeleteAction string `toml:"soft_delete_action,omitempty"`
}

// Schema is a parsed forge schema. Use ResolvedStorage() to obtain the
// effective storage even when the [storage] block was omitted by a legacy
// schema (the Rust resolved_storage() port).
type Schema struct {
	SupportedOps []string   `toml:"supported_ops,omitempty"`
	Hooks        []HookSpec `toml:"hooks,omitempty"`
	Meta         Metadata   `toml:"schema"`
	Storage      *Storage   `toml:"storage,omitempty"`
	Lifecycle    Lifecycle  `toml:"lifecycle,omitempty"`
	Fields       []Field    `toml:"fields"`
	Sections     []Section  `toml:"sections,omitempty"`
}

// ResolvedStorage returns the effective storage block. When the schema
// omits an explicit [storage], a Markdown-target storage is synthesized
// from the legacy top-level [schema] metadata + [[sections]] list.
func (s *Schema) ResolvedStorage() Storage {
	if s.Storage != nil {
		out := *s.Storage
		// Back-fill markdown sections from top-level when [storage.markdown]
		// declared no sections of its own.
		switch out.Target {
		case StorageTargetMarkdown:
			// flat form: no sub-struct to fill
		case StorageTargetDual:
			if out.Markdown != nil && len(out.Markdown.Sections) == 0 {
				out.Markdown.Sections = append([]Section(nil), s.Sections...)
			}
		}
		return out
	}
	return Storage{
		Target:          StorageTargetMarkdown,
		Prefix:          s.Meta.Prefix,
		OutputDir:       s.Meta.OutputDir,
		FilenamePattern: s.Meta.FilenamePattern,
	}
}

// FieldByName returns the named field definition, or (zero, false).
func (s *Schema) FieldByName(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Entry is a Schema plus the on-disk file it loaded from. The Registry
// surfaces these for tooling that needs to report sources.
type Entry struct {
	Schema     Schema
	SourceDir  string
	SourceFile string
	IsDraft    bool
}

// ParseError records a schema file that existed on disk but failed to load.
// Surfaced via Registry.ParseErrors() so misconfigurations show up at
// startup rather than as silent "schema not found" misses later.
type ParseError struct {
	SourceDir  string
	SourceFile string
	IsDraft    bool
	Err        string
}

// ReservedTopLevelKeys lists every key recognised by HandleForge at the
// top level of a sugar-shape params map. A schema field whose name matches
// one of these keys is silently stripped on the sugar shape — the field
// validation pass then reports it as "required field missing" even though
// the caller passed it. Schema authors must rename colliding fields (or
// callers must use the structured `fields: {…}` shape, which skips the
// strip). Surface via Registry.Warnings() so the footgun is visible at
// load time rather than at every failed forge call.
//
// Mirrors forgeReserved in internal/forge/handler.go — both pull from this
// list so the contract between authoring and dispatch stays single-sourced.
//
// `title` is intentionally NOT here: it is a real field name on several
// schemas (bug, vault-note) and HandleForge only reads it as a fallback
// for slug derivation when no slug is supplied.
var ReservedTopLevelKeys = map[string]struct{}{
	"schema_name": {},
	"kind":        {},
	"slug":        {},
	"project":     {},
	"id":          {},
	"date":        {},
	"commit_sha":  {},
	"fields":      {},
	// qwen_task_id is intentionally NOT reserved: it is a real bug.toml field
	// (originating qwen task attribution) and HandleForge has no envelope-level
	// handling for it (the BugReported payload reads the field directly). It was
	// reserved by mistake, which both warned at load and stripped the field on
	// the sugar shape (bug forge-bug-schema-field-qwen-task-id-collides-with-
	// handleforge-top-level-alias).
	// allow_placeholder is a forge_edit-only meta-param that opts out of
	// the placeholder-shape guard (rejects whole-value `{{NAME}}` literals
	// to prevent destructive overwrites from dry-run probes). Reserved
	// here so the sugar-shape strip recognises it across every schema.
	// See suggestion `forge-edit-reject-placeholder-shaped-values-by-default`.
	"allow_placeholder": {},
	// __drop_extras is a forge_edit-only meta-param naming non-declared
	// frontmatter keys to remove on rewrite. Reserved here so the sugar-
	// shape strip recognises it across every schema; forge create paths
	// silently ignore it (no-op on create). Bug
	// forge-edit-non-declared-frontmatter-keys-cant-be-dropped-or-renamed-on-relocate.
	"__drop_extras": {},
}

// Warning records a non-fatal authoring issue caught at schema load time.
// Schemas that produce warnings still register; the issue is surfaced via
// Registry.Warnings() so dashboards/CLIs can show authors what's wrong
// without breaking the server.
type Warning struct {
	SourceDir  string
	SourceFile string
	IsDraft    bool
	SchemaName string
	Kind       string // e.g. "field-name-reserved"
	Msg        string
}

// Conflict reports the second registration of a schema name. The first
// registration wins; the second is rejected.
type Conflict struct {
	Name         string
	WinningDir   string
	RejectedDir  string
	RejectedFile string
}

func (c Conflict) Error() string {
	return fmt.Sprintf(
		"schema %q registered by both %q and %q; second registration rejected",
		c.Name, c.WinningDir, c.RejectedDir,
	)
}

// Registry is a flat schema namespace assembled from one or more directories.
// Concurrent reads via Get/All are safe; Register/Reload take a write lock.
type Registry struct {
	mu          sync.RWMutex
	dirs        []string // registration order; needed for deterministic Reload
	entries     map[string]Entry
	sources     map[string]string // schema name → dir that won registration
	parseErrors []ParseError
	conflicts   []Conflict
	warnings    []Warning
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		entries: make(map[string]Entry),
		sources: make(map[string]string),
	}
}

// Load registers a single directory and returns the resulting registry.
// Convenience constructor for the single-directory case.
func Load(dir string) (*Registry, error) {
	r := New()
	if _, err := r.Register(dir); err != nil {
		return nil, err
	}
	return r, nil
}

// Register loads every *.toml file in dir (and dir/drafts/ if present) and
// merges successful schemas into the registry. Returns any name conflicts
// (the existing entry stays winning). Parse failures are captured into
// ParseErrors() rather than returned as errors — a single broken file does
// not abort the whole registration.
func (r *Registry) Register(dir string) ([]Conflict, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("forge registry: stat %s: %w", dir, err)
	}

	// Record for Reload(). Allow re-registering the same dir without
	// duplicating it in the order list.
	if !containsString(r.dirs, dir) {
		r.dirs = append(r.dirs, dir)
	}

	conflicts := r.scanDir(dir, false)
	draftsDir := filepath.Join(dir, "drafts")
	if info, err := os.Stat(draftsDir); err == nil && info.IsDir() {
		conflicts = append(conflicts, r.scanDir(draftsDir, true)...)
	}
	r.conflicts = append(r.conflicts, conflicts...)
	return conflicts, nil
}

// scanDir is called with the write lock held.
func (r *Registry) scanDir(dir string, isDraft bool) []Conflict {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var conflicts []Conflict
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, f.Name())
		schema, perr := loadFile(path)
		if perr != nil {
			r.parseErrors = append(r.parseErrors, ParseError{
				SourceDir:  dir,
				SourceFile: f.Name(),
				IsDraft:    isDraft,
				Err:        perr.Error(),
			})
			continue
		}
		name := schema.Meta.Name
		if name == "" {
			r.parseErrors = append(r.parseErrors, ParseError{
				SourceDir:  dir,
				SourceFile: f.Name(),
				IsDraft:    isDraft,
				Err:        "schema.name is empty",
			})
			continue
		}
		if existingDir, ok := r.sources[name]; ok {
			conflicts = append(conflicts, Conflict{
				Name:         name,
				WinningDir:   existingDir,
				RejectedDir:  dir,
				RejectedFile: f.Name(),
			})
			continue
		}
		r.entries[name] = Entry{
			Schema:     schema,
			SourceDir:  dir,
			SourceFile: f.Name(),
			IsDraft:    isDraft,
		}
		r.sources[name] = dir
		for _, field := range schema.Fields {
			// `project` is a reserved alias on the forge sugar shape. A
			// schema that nonetheless declares a same-named field (legacy
			// shape; vault-note used to do this before chain
			// `forge-vault-note-schema-rework` renamed its field to
			// `scope`) gets the dispatcher's top-level project
			// auto-injected by HandleForge — so the warning would be
			// misleading. Skip the collision warning for that one name.
			if field.Name == "project" {
				continue
			}
			if _, hit := ReservedTopLevelKeys[field.Name]; hit {
				w := Warning{
					SourceDir:  dir,
					SourceFile: f.Name(),
					IsDraft:    isDraft,
					SchemaName: name,
					Kind:       "field-name-reserved",
					Msg: fmt.Sprintf(
						"field %q collides with HandleForge top-level alias %q; "+
							"sugar-shape callers will hit \"required field missing\". "+
							"Rename the field (e.g. note_kind) or require callers use the structured fields:{} shape.",
						field.Name, field.Name),
				}
				r.warnings = append(r.warnings, w)
				obs.L().Warn("forge registry: reserved field-name collision",
					slog.String("dir", dir),
					slog.String("file", f.Name()),
					slog.String("schema", name),
					slog.String("msg", w.Msg),
				)
			}
		}
	}
	return conflicts
}

// Get returns the named schema. The second return is false on a miss.
func (r *Registry) Get(name string) (Schema, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return Schema{}, false
	}
	return e.Schema, true
}

// Entry returns the loaded entry for a name, including its source file.
func (r *Registry) Entry(name string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	return e, ok
}

// Names returns all registered schema names in lexicographic order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	// Stable order so callers (e.g. forge_schemas listing) see the same
	// sequence on repeated calls within a process.
	sortStrings(names)
	return names
}

// All returns every loaded entry. The slice is a fresh copy; callers may
// retain it without holding the registry lock.
func (r *Registry) All() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.entries))
	for _, n := range r.sortedNames() {
		out = append(out, r.entries[n])
	}
	return out
}

// ParseErrors returns every parse failure seen across registration calls.
// Cleared and rebuilt on Reload().
func (r *Registry) ParseErrors() []ParseError {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ParseError(nil), r.parseErrors...)
}

// Conflicts returns every conflict seen across registration calls.
func (r *Registry) Conflicts() []Conflict {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Conflict(nil), r.conflicts...)
}

// Warnings returns every non-fatal authoring issue caught at load time.
// Cleared and rebuilt on Reload(), same lifecycle as ParseErrors.
func (r *Registry) Warnings() []Warning {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Warning(nil), r.warnings...)
}

// Len reports the number of loaded schemas.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// Reload re-scans every previously-registered directory and atomically
// replaces the in-memory state. New TOML files appear; deleted files
// disappear; parse-error and conflict lists are rebuilt from scratch.
//
// Failure to stat a previously-registered directory aborts the reload
// with the entries/errors lists unchanged so a transient FS error does
// not blank the registry.
func (r *Registry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, d := range r.dirs {
		if _, err := os.Stat(d); err != nil {
			return fmt.Errorf("forge registry: reload: %w", err)
		}
	}

	r.entries = make(map[string]Entry)
	r.sources = make(map[string]string)
	r.parseErrors = nil
	r.conflicts = nil
	r.warnings = nil

	for _, d := range r.dirs {
		conflicts := r.scanDir(d, false)
		draftsDir := filepath.Join(d, "drafts")
		if info, err := os.Stat(draftsDir); err == nil && info.IsDir() {
			conflicts = append(conflicts, r.scanDir(draftsDir, true)...)
		}
		r.conflicts = append(r.conflicts, conflicts...)
	}
	return nil
}

// loadFile parses a schema TOML and applies load-time validation.
func loadFile(path string) (Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Schema{}, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	var schema Schema
	if _, err := toml.Decode(string(data), &schema); err != nil {
		return Schema{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if err := validate(&schema); err != nil {
		return Schema{}, fmt.Errorf("validate %s: %w", filepath.Base(path), err)
	}
	return schema, nil
}

// validate is the load-time structural check. It rejects unknown field
// types, missing enum_values on enum-shaped fields, and uncompilable
// patterns. Deeper convention checks (cross-field requires, foreign-key
// reachability, db-identifier safety) are layered on top in subsequent
// tasks (T49+).
func validate(s *Schema) error {
	if s.Meta.Name == "" {
		return fmt.Errorf("schema.name is required")
	}
	if len(s.Fields) == 0 {
		return fmt.Errorf("schema %q declares no fields", s.Meta.Name)
	}
	seen := make(map[string]struct{}, len(s.Fields))
	for i, f := range s.Fields {
		if f.Name == "" {
			return fmt.Errorf("field[%d]: name is required", i)
		}
		if _, dup := seen[f.Name]; dup {
			return fmt.Errorf("field %q declared twice", f.Name)
		}
		seen[f.Name] = struct{}{}
		if !f.Type.known() {
			return fmt.Errorf("field %q: unknown type %q", f.Name, f.Type)
		}
		if len(f.EnumValues) > 0 {
			for _, v := range f.EnumValues {
				if v == "" {
					return fmt.Errorf("field %q: enum_values contains an empty string", f.Name)
				}
			}
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return fmt.Errorf("field %q: pattern is not a valid regex: %w", f.Name, err)
			}
		}
	}
	if s.Storage != nil {
		st := s.Storage
		switch st.Target {
		case StorageTargetMarkdown, StorageTargetDB, StorageTargetDual:
			// known
		case "":
			return fmt.Errorf("schema %q: storage.target is required when [storage] is declared", s.Meta.Name)
		default:
			return fmt.Errorf("schema %q: unknown storage target %q", s.Meta.Name, st.Target)
		}
		if st.Target == StorageTargetDB || st.Target == StorageTargetDual {
			table := st.Table
			if st.Target == StorageTargetDual && st.DB != nil {
				table = st.DB.Table
			}
			if table == "" {
				return fmt.Errorf("schema %q: db-backed storage requires a table name", s.Meta.Name)
			}
		}
	}
	return nil
}

func containsString(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

func sortStrings(ss []string) {
	// Insertion sort — registry sizes are tiny (~10 schemas).
	for i := 1; i < len(ss); i++ {
		j := i
		for j > 0 && ss[j-1] > ss[j] {
			ss[j-1], ss[j] = ss[j], ss[j-1]
			j--
		}
	}
}

func (r *Registry) sortedNames() []string {
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}
