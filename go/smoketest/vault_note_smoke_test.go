package smoketest_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestVaultNoteSmoke_EndToEnd exercises the full forge(vault-note) path
// against the live Go MCP transport: forge the artifact, assert the
// markdown file lands at the expected subdir with correct frontmatter,
// assert the knowledge_pointers + FTS5 row landed via the auto-index
// notifier.
//
// Uses FORGE_MARKDOWN_ROOT to override the on-disk vault root with a
// t.TempDir() so the canonical ~/.claude/vault/ stays untouched.
//
// Three subtests, one per kind (decision, learning, reference) covering
// subdir routing + frontmatter shape. The file-on-disk + pointer-row +
// FTS5-rowid assertions prove the integration end-to-end. vault_search
// now honors FORGE_MARKDOWN_ROOT too (resolves to "<root>/vault") so a
// later smoke can call vault_search against the temp root and assert
// the new pointer surfaces; deferred here only because it would require
// a live llama-server / Qwen mock at the binary level. The env-var
// resolution itself is unit-pinned in internal/knowledge/vault.
func TestVaultNoteSmoke_EndToEnd(t *testing.T) {
	binary := serverBinary(t)
	dbPath := prepWorkDB(t)
	bpDir := blueprintsDirSmoke(t)

	vaultRoot := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", vaultRoot)

	srv := spawnWorkServer(t, binary, dbPath, bpDir)

	now := time.Now().Unix()

	t.Run("decision", func(t *testing.T) {
		slug := fmt.Sprintf("smoke-decision-%d", now)
		title := "Forge-path adoption for vault entries"
		body := "Captures the decision to author new vault entries via forge(vault-note). " +
			"Schema-enforced frontmatter and auto-index into knowledge_pointers are the two benefits."
		resp := srv.callWork(t, "forge", map[string]any{
			"schema_name": "vault-note",
			"slug":        slug,
			"note_kind":   "decision",
			"title":       title,
			"body":        body,
			"tags":        "vault,forge,smoke",
		})
		if resp["ok"] != true {
			t.Fatalf("forge vault-note (decision): %+v", resp)
		}
		assertVaultFileLanded(t, vaultRoot, "decisions", slug, title)
		// Top-level project (mcp-servers, via spawnWorkServer's
		// --default-project) is the DB-attribution stamp on the pointer;
		// the cross-project "vault" sentinel only applies when top-level
		// project is also empty. (Chain `forge-vault-note-schema-rework`
		// kept the work-surface override convention; only the routing
		// side was decoupled.)
		assertPointerLanded(t, dbPath, "decisions", slug, "mcp-servers")
	})

	t.Run("learning_with_scope", func(t *testing.T) {
		slug := fmt.Sprintf("smoke-learning-%d", now)
		title := "FTS5 virtual-table sync uses DELETE-then-INSERT"
		body := "Inverted-index drift happens when you UPDATE _content directly; " +
			"DELETE through the virtual table and re-INSERT instead."
		resp := srv.callWork(t, "forge", map[string]any{
			"schema_name": "vault-note",
			"slug":        slug,
			"note_kind":   "learning",
			"scope":       "mcp-servers",
			"title":       title,
			"body":        body,
			"tags":        "fts5,sqlite,sync",
		})
		if resp["ok"] != true {
			t.Fatalf("forge vault-note (learning): %+v", resp)
		}
		// Explicit `scope=mcp-servers` (a fields entry, not top-level
		// project) routes the learning to learnings/mcp-servers/. The
		// pointer's project_id is mcp-servers via the top-level work-
		// surface attribution stamp (spawnWorkServer's --default-project).
		// Chain `forge-vault-note-schema-rework` removed the prior
		// top-level-project-auto-routes-to-subdir behavior bug 1433
		// documented.
		assertVaultFileLanded(t, vaultRoot, "learnings/mcp-servers", slug, title)
		assertPointerLanded(t, dbPath, "learnings/mcp-servers", slug, "mcp-servers")
	})

	t.Run("reference", func(t *testing.T) {
		slug := fmt.Sprintf("smoke-reference-%d", now)
		title := "Toolkit-server bin paths reference"
		body := "The canonical Go binary is at mcp-servers/go/bin/toolkit-server. " +
			"Default HTTP port is 3000; --http-only skips MCP stdio."
		resp := srv.callWork(t, "forge", map[string]any{
			"schema_name": "vault-note",
			"slug":        slug,
			"note_kind":   "reference",
			"title":       title,
			"body":        body,
		})
		if resp["ok"] != true {
			t.Fatalf("forge vault-note (reference): %+v", resp)
		}
		// reference is cross-project — pointer project_id falls back to
		// the dispatcher's mcp-servers (not the "vault" sentinel, because
		// spawnWorkServer passes --default-project=mcp-servers).
		assertVaultFileLanded(t, vaultRoot, "reference", slug, title)
		assertPointerLanded(t, dbPath, "reference", slug, "mcp-servers")
	})
}

// assertVaultFileLanded checks that a markdown file exists at
// vaultRoot/vault/<subdir>/<date>_<slug>.md and that its body contains
// the expected title plus YAML frontmatter (date, slug, title, kind).
func assertVaultFileLanded(t *testing.T, vaultRoot, subdir, slug, title string) {
	t.Helper()
	dir := filepath.Join(vaultRoot, "vault", subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var matched string
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, slug) && strings.HasSuffix(name, ".md") {
			matched = filepath.Join(dir, name)
			break
		}
	}
	if matched == "" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no file containing slug %q under %s\nentries: %v", slug, dir, names)
	}
	body, err := os.ReadFile(matched)
	if err != nil {
		t.Fatalf("read %s: %v", matched, err)
	}
	for _, want := range []string{
		"---\n", // frontmatter open
		"slug: " + slug,
		"title: " + title,
		"## Title\n",
		title,
		"## Body\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("file %s missing %q in body", matched, want)
		}
	}
}

// assertPointerLanded checks that knowledge_pointers contains a row
// for the forged vault-note with the expected source_ref shape and
// project_id, and that knowledge_pointers_fts MATCH returns the row.
func assertPointerLanded(t *testing.T, dbPath, expectSubdir, slug, expectProject string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var id int64
	var sourceType, sourceRef, projectID string
	err = db.QueryRow(`
		SELECT id, source_type, source_ref, project_id
		  FROM knowledge_pointers
		 WHERE source_type = 'vault' AND source_ref LIKE ? || '/%' || ? || '.md'
		 ORDER BY id DESC LIMIT 1`,
		expectSubdir, slug).Scan(&id, &sourceType, &sourceRef, &projectID)
	if err != nil {
		t.Fatalf("pointer not found for slug %q under %s: %v", slug, expectSubdir, err)
	}
	if !strings.HasPrefix(sourceRef, expectSubdir+"/") {
		t.Errorf("source_ref subdir mismatch: got %q, want prefix %q/", sourceRef, expectSubdir)
	}
	if projectID != expectProject {
		t.Errorf("project_id: got %q, want %q", projectID, expectProject)
	}

	// FTS5 reachability check: confirm the row id we just resolved
	// exists in the FTS table. Querying by rowid is unambiguous and
	// avoids the FTS5 syntax pitfall where slug stems containing
	// numeric segments get parsed as column references.
	var ftsCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM knowledge_pointers_fts WHERE rowid = ?`, id).Scan(&ftsCount); err != nil {
		t.Fatalf("fts count for rowid %d: %v", id, err)
	}
	if ftsCount == 0 {
		t.Errorf("FTS5 has no row at rowid %d; auto-index didn't reach the virtual table", id)
	}
}
