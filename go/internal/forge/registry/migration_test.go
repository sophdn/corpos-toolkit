package registry_test

import (
	"strings"
	"testing"

	"toolkit/internal/forge/registry"
)

// The forge(migration) schema registers with the rest of the schema set
// at startup (blueprints/forge-schemas/migration.toml). Verifying it
// loads + exposes the expected fields and storage shape catches a
// schema-author regression at the cheapest layer; the behavior smoke
// rows in go/internal/forge/migration_test.go cover the create handler.
func TestRegistry_MigrationSchemaLoads(t *testing.T) {
	dir := findRepoSchemas(t)
	if dir == "" {
		t.Skip("blueprints/forge-schemas not found relative to test CWD")
	}
	r := mustRegister(t, dir)

	schema, ok := r.Get("migration")
	if !ok {
		t.Fatalf("migration schema not registered; got %v", r.Names())
	}

	if got, want := schema.Meta.Name, "migration"; got != want {
		t.Errorf("schema.name: got %q, want %q", got, want)
	}
	if got, want := schema.Meta.OutputDir, "go/internal/db/migrations"; got != want {
		t.Errorf("schema.output_dir: got %q, want %q", got, want)
	}
	if !strings.Contains(schema.Meta.FilenamePattern, "{migration_number}") {
		t.Errorf("schema.filename_pattern: got %q, want a {migration_number} placeholder",
			schema.Meta.FilenamePattern)
	}

	storage := schema.ResolvedStorage()
	if got, want := storage.Target, registry.StorageTargetMarkdown; got != want {
		t.Errorf("storage.target: got %q, want %q", got, want)
	}

	upSQL, hasUpSQL := schema.FieldByName("up_sql")
	if !hasUpSQL {
		t.Fatal("schema missing required field up_sql")
	}
	if !upSQL.Type.IsRequired() {
		t.Errorf("up_sql.type %q: must be required", upSQL.Type)
	}
	if _, hasDocstring := schema.FieldByName("docstring"); !hasDocstring {
		t.Error("schema missing optional field docstring")
	}
}
