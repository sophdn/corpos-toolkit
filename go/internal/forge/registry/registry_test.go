package registry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/forge/registry"
)

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func mustRegister(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	r, err := registry.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestRegistry_LoadsMarkdownAndDBSchemas(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(dir, "alpha.toml"))
	copyFile(t, "testdata/db-only.toml", filepath.Join(dir, "beta.toml"))

	r := mustRegister(t, dir)

	if r.Len() != 2 {
		t.Errorf("Len: want 2, got %d", r.Len())
	}

	alpha, ok := r.Get("alpha")
	if !ok {
		t.Fatal("alpha not found")
	}
	if alpha.Meta.Prefix != "ALPHA" {
		t.Errorf("alpha prefix: want ALPHA, got %q", alpha.Meta.Prefix)
	}
	if alpha.Storage == nil || alpha.Storage.Target != registry.StorageTargetMarkdown {
		t.Errorf("alpha storage target: want markdown, got %+v", alpha.Storage)
	}
	if len(alpha.Sections) != 2 {
		t.Errorf("alpha sections: want 2, got %d", len(alpha.Sections))
	}

	beta, ok := r.Get("beta")
	if !ok {
		t.Fatal("beta not found")
	}
	if beta.Storage == nil || beta.Storage.Target != registry.StorageTargetDB {
		t.Errorf("beta storage target: want db, got %+v", beta.Storage)
	}
	if beta.Storage.Table != "betas" {
		t.Errorf("beta storage table: want betas, got %q", beta.Storage.Table)
	}
	chainField, ok := beta.FieldByName("chain_slug")
	if !ok {
		t.Fatal("beta.chain_slug not found")
	}
	if chainField.ForeignKey == nil || chainField.ForeignKey.Shape != "chain" {
		t.Errorf("beta.chain_slug foreign_key: want shape=chain, got %+v", chainField.ForeignKey)
	}
	statusField, _ := beta.FieldByName("status")
	if len(statusField.EnumValues) != 2 {
		t.Errorf("beta.status enum_values: want 2, got %d", len(statusField.EnumValues))
	}
}

func TestRegistry_GetMiss(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(dir, "alpha.toml"))

	r := mustRegister(t, dir)
	if _, ok := r.Get("no-such-schema"); ok {
		t.Error("expected Get miss for unknown schema")
	}
}

func TestRegistry_RejectsUnknownFieldType(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/unknown-type.toml", filepath.Join(dir, "broken.toml"))

	r := mustRegister(t, dir)
	if _, ok := r.Get("broken"); ok {
		t.Error("expected broken schema to be excluded after parse failure")
	}
	errs := r.ParseErrors()
	if len(errs) != 1 {
		t.Fatalf("ParseErrors: want 1, got %d", len(errs))
	}
	if errs[0].SourceFile != "broken.toml" {
		t.Errorf("ParseErrors[0].SourceFile: want broken.toml, got %q", errs[0].SourceFile)
	}
}

func TestRegistry_RejectsBadPattern(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/bad-pattern.toml", filepath.Join(dir, "bad-pattern.toml"))

	r := mustRegister(t, dir)
	if _, ok := r.Get("bad-pattern"); ok {
		t.Error("expected bad-pattern schema to be excluded")
	}
	errs := r.ParseErrors()
	if len(errs) != 1 {
		t.Fatalf("ParseErrors: want 1, got %d", len(errs))
	}
}

// TestRegistry_WarnsOnReservedFieldName pins the footgun-detector: a schema
// that declares a field whose name overlaps with a HandleForge top-level
// alias (kind, slug, project, date, id, …) still registers, but surfaces
// a Warning at load time so authors see the issue before sugar-shape
// callers hit a confusing "required field missing".
func TestRegistry_WarnsOnReservedFieldName(t *testing.T) {
	dir := t.TempDir()
	body := `supported_ops = ["create"]

[schema]
name = "reserved-collision"
prefix = "RC"
output_dir = "notes/rc"
filename_pattern = "{prefix}_{slug}_{date}.md"

[storage]
target = "markdown"
prefix = "RC"
output_dir = "notes/rc"
filename_pattern = "{prefix}_{slug}_{date}.md"

[[fields]]
name = "kind"
type = "string"
description = "Type of entry — collides with HandleForge kind alias."

[[fields]]
name = "body"
type = "string"
description = "Body of the note."

[[sections]]
heading = "Body"
fields = ["body"]
`
	if err := os.WriteFile(filepath.Join(dir, "rc.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write rc.toml: %v", err)
	}

	r := mustRegister(t, dir)

	if _, ok := r.Get("reserved-collision"); !ok {
		t.Fatal("schema should still register; warning is non-fatal")
	}
	warns := r.Warnings()
	if len(warns) != 1 {
		t.Fatalf("Warnings: want 1, got %d (%+v)", len(warns), warns)
	}
	w := warns[0]
	if w.SchemaName != "reserved-collision" {
		t.Errorf("SchemaName: want reserved-collision, got %q", w.SchemaName)
	}
	if w.Kind != "field-name-reserved" {
		t.Errorf("Kind: want field-name-reserved, got %q", w.Kind)
	}
	if !strings.Contains(w.Msg, `"kind"`) {
		t.Errorf("Msg should name the colliding field; got %q", w.Msg)
	}
}

// TestRegistry_NoWarningOnProjectField pins the deliberate exclusion: a
// schema that declares a `project` field is OK because
// schemaDeclaresProject + the HandleForge injector re-binds it for
// sugar-shape callers. The warning is silenced so it doesn't fire on
// every server startup for the production vault-note schema.
func TestRegistry_NoWarningOnProjectField(t *testing.T) {
	dir := t.TempDir()
	body := `supported_ops = ["create"]

[schema]
name = "with-project"
prefix = "WP"
output_dir = "notes/wp"
filename_pattern = "{prefix}_{slug}_{date}.md"

[storage]
target = "markdown"
prefix = "WP"
output_dir = "notes/wp"
filename_pattern = "{prefix}_{slug}_{date}.md"

[[fields]]
name = "project"
type = "string"
description = "Cross-project pointer — injected by HandleForge."

[[fields]]
name = "body"
type = "string"
description = "Body."

[[sections]]
heading = "Body"
fields = ["body"]
`
	if err := os.WriteFile(filepath.Join(dir, "wp.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write wp.toml: %v", err)
	}
	r := mustRegister(t, dir)
	if len(r.Warnings()) != 0 {
		t.Errorf("project field should be silently allowed; got %+v", r.Warnings())
	}
}

// TestRegistry_WarnsForEveryReservedField exercises a schema with two
// colliding fields to confirm the detector enumerates rather than
// short-circuiting after the first hit.
func TestRegistry_WarnsForEveryReservedField(t *testing.T) {
	dir := t.TempDir()
	body := `supported_ops = ["create"]

[schema]
name = "two-collisions"
prefix = "TC"
output_dir = "notes/tc"
filename_pattern = "{prefix}_{slug}_{date}.md"

[storage]
target = "markdown"
prefix = "TC"
output_dir = "notes/tc"
filename_pattern = "{prefix}_{slug}_{date}.md"

[[fields]]
name = "slug"
type = "string"
description = "Collides with slug alias."

[[fields]]
name = "date"
type = "string"
description = "Collides with date alias."

[[fields]]
name = "body"
type = "string"
description = "Body."

[[sections]]
heading = "Body"
fields = ["body"]
`
	if err := os.WriteFile(filepath.Join(dir, "tc.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write tc.toml: %v", err)
	}
	r := mustRegister(t, dir)
	warns := r.Warnings()
	if len(warns) != 2 {
		t.Fatalf("Warnings: want 2, got %d (%+v)", len(warns), warns)
	}
	got := map[string]bool{}
	for _, w := range warns {
		got[w.Msg] = true
	}
	if len(got) != 2 {
		t.Errorf("expected two distinct messages, got %+v", got)
	}
}

func TestRegistry_ReloadPicksUpNewFile(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(dir, "alpha.toml"))

	r := mustRegister(t, dir)
	if r.Len() != 1 {
		t.Fatalf("initial Len: want 1, got %d", r.Len())
	}

	copyFile(t, "testdata/db-only.toml", filepath.Join(dir, "beta.toml"))
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if r.Len() != 2 {
		t.Errorf("post-reload Len: want 2, got %d", r.Len())
	}
	if _, ok := r.Get("beta"); !ok {
		t.Error("beta not visible after Reload")
	}
}

func TestRegistry_ReloadDropsDeletedFile(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(dir, "alpha.toml"))
	copyFile(t, "testdata/db-only.toml", filepath.Join(dir, "beta.toml"))

	r := mustRegister(t, dir)
	if r.Len() != 2 {
		t.Fatalf("initial Len: want 2, got %d", r.Len())
	}

	if err := os.Remove(filepath.Join(dir, "beta.toml")); err != nil {
		t.Fatalf("remove beta.toml: %v", err)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("post-reload Len: want 1, got %d", r.Len())
	}
	if _, ok := r.Get("beta"); ok {
		t.Error("beta still visible after deletion + reload")
	}
}

func TestRegistry_ConflictBetweenDirs(t *testing.T) {
	core := t.TempDir()
	project := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(core, "alpha.toml"))
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(project, "alpha.toml"))

	r := registry.New()
	c1, err := r.Register(core)
	if err != nil {
		t.Fatalf("Register core: %v", err)
	}
	if len(c1) != 0 {
		t.Errorf("core registration: want 0 conflicts, got %d", len(c1))
	}
	c2, err := r.Register(project)
	if err != nil {
		t.Fatalf("Register project: %v", err)
	}
	if len(c2) != 1 {
		t.Fatalf("project registration: want 1 conflict, got %d", len(c2))
	}
	if c2[0].Name != "alpha" {
		t.Errorf("conflict name: want alpha, got %q", c2[0].Name)
	}
	if c2[0].WinningDir != core {
		t.Errorf("WinningDir: want %q, got %q", core, c2[0].WinningDir)
	}

	// Only one alpha is registered; the winning one is from core.
	if r.Len() != 1 {
		t.Errorf("Len: want 1, got %d", r.Len())
	}
}

func TestRegistry_DraftsDirIsScanned(t *testing.T) {
	dir := t.TempDir()
	draftsDir := filepath.Join(dir, "drafts")
	if err := os.MkdirAll(draftsDir, 0o700); err != nil {
		t.Fatalf("mkdir drafts: %v", err)
	}
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(draftsDir, "alpha.toml"))

	r := mustRegister(t, dir)
	entry, ok := r.Entry("alpha")
	if !ok {
		t.Fatal("alpha not found")
	}
	if !entry.IsDraft {
		t.Error("expected entry to be marked as draft")
	}
}

func TestRegistry_StableOrderingFromNamesAndAll(t *testing.T) {
	dir := t.TempDir()
	copyFile(t, "testdata/markdown-only.toml", filepath.Join(dir, "alpha.toml"))
	copyFile(t, "testdata/db-only.toml", filepath.Join(dir, "beta.toml"))
	copyFile(t, "testdata/with-pattern.toml", filepath.Join(dir, "gamma.toml"))

	r := mustRegister(t, dir)
	names := r.Names()
	if len(names) != 3 || names[0] != "alpha" || names[1] != "beta" || names[2] != "gamma" {
		t.Errorf("Names order: got %v", names)
	}

	entries := r.All()
	if len(entries) != 3 {
		t.Fatalf("All len: want 3, got %d", len(entries))
	}
	if entries[0].Schema.Meta.Name != "alpha" || entries[2].Schema.Meta.Name != "gamma" {
		t.Errorf("All order: got %v / %v", entries[0].Schema.Meta.Name, entries[2].Schema.Meta.Name)
	}
}

func TestRegistry_FieldTypePredicates(t *testing.T) {
	cases := []struct {
		ft           registry.FieldType
		wantRequired bool
		wantList     bool
	}{
		{registry.FieldTypeString, true, false},
		{registry.FieldTypeStringList, true, true},
		{registry.FieldTypeOptionalString, false, false},
		{registry.FieldTypeOptionalStringList, false, true},
		{registry.FieldTypeStringOrList, true, true},
		{registry.FieldTypeOptionalStringOrList, false, true},
	}
	for _, c := range cases {
		t.Run(string(c.ft), func(t *testing.T) {
			if c.ft.IsRequired() != c.wantRequired {
				t.Errorf("IsRequired %q: want %v", c.ft, c.wantRequired)
			}
			if c.ft.IsList() != c.wantList {
				t.Errorf("IsList %q: want %v", c.ft, c.wantList)
			}
		})
	}
}

func TestRegistry_ResolvedStorageSynthesizedFromLegacy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.toml"), []byte(`
[schema]
name = "legacy"
prefix = "LEGACY"
output_dir = "notes/legacy"
filename_pattern = "{prefix}_{slug}.md"

[[fields]]
name = "body"
type = "string"
description = "Body."

[[sections]]
heading = "Body"
fields = ["body"]
`), 0o600); err != nil {
		t.Fatalf("write legacy.toml: %v", err)
	}

	r := mustRegister(t, dir)
	s, ok := r.Get("legacy")
	if !ok {
		t.Fatal("legacy not found")
	}
	resolved := s.ResolvedStorage()
	if resolved.Target != registry.StorageTargetMarkdown {
		t.Errorf("resolved target: want markdown, got %q", resolved.Target)
	}
	if resolved.Prefix != "LEGACY" {
		t.Errorf("resolved prefix: want LEGACY, got %q", resolved.Prefix)
	}
}

func TestRegistry_RealRepoSchemasLoad(t *testing.T) {
	// Sanity: the live blueprints directory loads without parse errors.
	// Locates the schemas relative to the module by walking up from the
	// test file's CWD until a `blueprints/forge-schemas` directory exists.
	dir := findRepoSchemas(t)
	if dir == "" {
		t.Skip("blueprints/forge-schemas not found relative to test CWD")
	}
	r := mustRegister(t, dir)
	if errs := r.ParseErrors(); len(errs) > 0 {
		for _, e := range errs {
			t.Logf("parse error: %s/%s — %s", e.SourceDir, e.SourceFile, e.Err)
		}
		t.Fatalf("ParseErrors: want 0, got %d", len(errs))
	}
	for _, want := range []string{"chain", "task", "bug", "suggestion"} {
		if _, ok := r.Get(want); !ok {
			t.Errorf("real-repo schema %q not loaded", want)
		}
	}
}

func findRepoSchemas(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for d := wd; d != "/" && d != ""; d = filepath.Dir(d) {
		candidate := filepath.Join(d, "blueprints", "forge-schemas")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}
