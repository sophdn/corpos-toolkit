package construct_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"toolkit/internal/construct"
	"toolkit/internal/db"
)

func readMemoryRow(t *testing.T, pool *db.Pool, name string) (kind, description string, bodyLen int, vaultPath string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT kind, description, body_length_bytes, vault_path FROM proj_memories WHERE name = ?`, name,
	).Scan(&kind, &description, &bodyLen, &vaultPath); err != nil {
		t.Fatalf("read memory %q: %v", name, err)
	}
	return
}

// TestCreateForgeMemoryParity proves the FILE-schema pattern (bucket 4):
// construct.Create("memory", ...) writes a file via forge.WriteMemoryArtifact
// + emits MemoryWritten through record, producing a byte-identical FILE to
// forge(memory) (modulo the slug-derived `name:` line) and a matching
// proj_memories row. CreateResult.FilePath reports the on-disk path.
func TestCreateForgeMemoryParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	reg := loadForgeRegistry(t)
	const kind = "project"
	const desc = "the construct umbrella covers the memory file schema"
	const body = "Lead with the rule.\n\n**Why:** file is the artifact.\n**How to apply:** re-home forge's renderer."

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "memory", "slug": "mem-forge", "name": "mem-forge", "memory_kind": kind,
		"description": desc, "body": body,
	})

	deps := construct.Deps{Pool: pool, Schemas: reg}
	res, err := construct.Create(ctx, deps, "memory", "mcp-servers", construct.Input{
		Memory: &construct.MemoryInput{Slug: "mem-record", Kind: kind, Description: desc, Body: body},
	})
	if err != nil {
		t.Fatalf("Create(memory): %v", err)
	}
	dir := filepath.Join(root, "vault", "memory", kind)
	if res.FilePath != filepath.Join(dir, "mem-record.md") {
		t.Fatalf("CreateResult.FilePath=%q, want the on-disk path", res.FilePath)
	}

	fBytes, err := os.ReadFile(filepath.Join(dir, "mem-forge.md"))
	if err != nil {
		t.Fatalf("read forge memory file: %v", err)
	}
	rBytes, err := os.ReadFile(filepath.Join(dir, "mem-record.md"))
	if err != nil {
		t.Fatalf("read construct memory file: %v", err)
	}
	fNorm := strings.Replace(string(fBytes), "name: mem-forge", "name: <slug>", 1)
	rNorm := strings.Replace(string(rBytes), "name: mem-record", "name: <slug>", 1)
	if fNorm != rNorm {
		t.Fatalf("memory file parity mismatch:\n--- forge ---\n%s\n--- construct ---\n%s", fNorm, rNorm)
	}

	fKind, fDesc, fLen, _ := readMemoryRow(t, pool, "mem-forge")
	rKind, rDesc, rLen, rPath := readMemoryRow(t, pool, "mem-record")
	if fKind != rKind || fDesc != rDesc || fLen != rLen {
		t.Fatalf("memory projection parity mismatch:\n  forge:     kind=%q desc=%q len=%d\n  construct: kind=%q desc=%q len=%d",
			fKind, fDesc, fLen, rKind, rDesc, rLen)
	}
	if rPath != filepath.Join(dir, "mem-record.md") {
		t.Fatalf("construct memory vault_path=%q, want the on-disk path", rPath)
	}
}

// readOnlyDoc reads the single .md file under <root>/docs.
func readOnlyDoc(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "docs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read docs dir %s: %v", dir, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read doc %s: %v", e.Name(), err)
			}
			return string(b)
		}
	}
	t.Fatalf("no .md doc under %s", dir)
	return ""
}

func readOnlyPointerFields(t *testing.T, pool *db.Pool, sourceType string) (question, descr, tags string, quality float64, sourceRef string) {
	t.Helper()
	if err := pool.DB().QueryRow(
		`SELECT question, COALESCE(description,''), tags, COALESCE(quality_score,0), source_ref
		   FROM knowledge_pointers WHERE source_type = ?`, sourceType,
	).Scan(&question, &descr, &tags, &quality, &sourceRef); err != nil {
		t.Fatalf("read %s pointer: %v", sourceType, err)
	}
	return
}

// TestCreateForgeChainAnchoredDocParity proves retrospective + report-card:
// construct.Create routes to buildRetrospective / buildReportCard via
// forge.WriteChainAnchoredDoc + event submission, producing a byte-identical
// file to forge + an identical knowledge_pointer.
func TestCreateForgeChainAnchoredDocParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)

	cases := []struct {
		name      string
		eventType string
		chainSlug string
		sections  map[string]string
	}{
		{name: "retrospective", eventType: "RetrospectiveForged", chainSlug: "cad-retro-chain",
			sections: map[string]string{"what_landed": "shipped X and Y", "surprises": "Z was harder"}},
		{name: "report-card", eventType: "ReportCardForged", chainSlug: "cad-rc-chain",
			sections: map[string]string{"per_task_grades": "T1: A", "overall_verdict": "SHIP"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chainRaw, _ := json.Marshal(map[string]any{"schema_name": "chain", "slug": tc.chainSlug,
				"output": "o", "design_decisions": "dd", "completion_condition": "cc"})
			if r, err := forgeCreateRaw(t, pool, "mcp-servers", chainRaw); err != nil || r.Error != "" {
				t.Fatalf("forge(chain): %v %q", err, r.Error)
			}

			rootF := t.TempDir()
			t.Setenv("FORGE_MARKDOWN_ROOT", rootF)
			fparams := map[string]any{"schema_name": tc.name, "slug": "doc-" + tc.name, "chain_slug": tc.chainSlug}
			for k, v := range tc.sections {
				fparams[k] = v
			}
			fraw, _ := json.Marshal(fparams)
			if r, err := forgeCreateRaw(t, pool, "mcp-servers", fraw); err != nil || r.Error != "" {
				t.Fatalf("forge(%s): %v %q", tc.name, err, r.Error)
			}
			fFile := readOnlyDoc(t, rootF)
			fq, fd, ft, fqual, fRef := readOnlyPointerFields(t, pool, tc.name)

			rootR := t.TempDir()
			t.Setenv("FORGE_MARKDOWN_ROOT", rootR)
			constructDeps := construct.Deps{Pool: pool, Schemas: reg}
			var in construct.Input
			doc := &construct.ChainAnchoredDocInput{Slug: "doc-" + tc.name, ChainSlug: tc.chainSlug, Sections: tc.sections}
			if tc.name == "retrospective" {
				in.Retrospective = doc
			} else {
				in.ReportCard = doc
			}
			res, err := construct.Create(ctx, constructDeps, tc.name, "mcp-servers", in)
			if err != nil {
				t.Fatalf("Create(%s): %v", tc.name, err)
			}
			rFile := readOnlyDoc(t, rootR)
			if res.FilePath == "" {
				t.Fatalf("CreateResult.FilePath empty for %s", tc.name)
			}

			if fFile != rFile {
				t.Fatalf("%s file parity mismatch:\n--- forge ---\n%s\n--- construct ---\n%s", tc.name, fFile, rFile)
			}
			rq, rd, rt, rqual, rRef := readOnlyPointerFields(t, pool, tc.name)
			if fq != rq || fd != rd || ft != rt || fqual != rqual || fRef != rRef {
				t.Fatalf("%s pointer parity mismatch:\n  forge:     q=%q d=%q tags=%q qual=%v ref=%q\n  construct: q=%q d=%q tags=%q qual=%v ref=%q",
					tc.name, fq, fd, ft, fqual, fRef, rq, rd, rt, rqual, rRef)
			}
		})
	}
}

// TestCreateForgeChainAnchoredDocRejectsMissingChain proves the umbrella
// surfaces chain_not_found from the underlying buildChainAnchoredDoc /
// forge.WriteChainAnchoredDoc rejection.
func TestCreateForgeChainAnchoredDocRejectsMissingChain(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)
	t.Setenv("FORGE_MARKDOWN_ROOT", t.TempDir())
	deps := construct.Deps{Pool: pool, Schemas: reg}
	_, err := construct.Create(ctx, deps, "retrospective", "mcp-servers", construct.Input{
		Retrospective: &construct.ChainAnchoredDocInput{
			Slug: "x", ChainSlug: "no-such-chain", Sections: map[string]string{"what_landed": "x"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "chain_not_found") {
		t.Fatalf("expected chain_not_found rejection, got: %v", err)
	}
}

// TestCreateRetrospectiveDoesNotFireCaptureOrphans pins the documented §15
// delta: forge(retrospective) runs captureOrphanedFollowons as a post-write
// fail-open hook (auto-files suggestions for next-chain candidates);
// construct.Create("retrospective", ...) does NOT. A future re-home accident
// wiring that hook into the construct path fails this test.
func TestCreateRetrospectiveDoesNotFireCaptureOrphans(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)
	t.Setenv("FORGE_MARKDOWN_ROOT", t.TempDir())

	// Seed the anchor chain.
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "chain", "slug": "capture-orphans-chain",
		"output": "o", "design_decisions": "dd", "completion_condition": "cc",
	})

	var before int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_current_suggestions`).Scan(&before); err != nil {
		t.Fatalf("count suggestions before: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: reg}
	_, err := construct.Create(ctx, deps, "retrospective", "mcp-servers", construct.Input{
		Retrospective: &construct.ChainAnchoredDocInput{
			Slug:      "no-capture-orphans",
			ChainSlug: "capture-orphans-chain",
			Sections: map[string]string{
				"what_landed": "shipped the thing",
				"surprises":   "next chain candidate: rewrite the dispatcher; another candidate: improve the index",
			},
		},
	})
	if err != nil {
		t.Fatalf("Create(retrospective): %v", err)
	}

	var after int
	if err := pool.DB().QueryRow(`SELECT COUNT(*) FROM proj_current_suggestions`).Scan(&after); err != nil {
		t.Fatalf("count suggestions after: %v", err)
	}
	if after != before {
		t.Fatalf("construct.Create(retrospective) leaked a suggestion: before=%d after=%d — captureOrphanedFollowons (or an equivalent hook) may have been wired into the construct path; this is the §15 documented delta", before, after)
	}
}

func readMigrationBody(t *testing.T, root, subdir, suffix string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, subdir, "*_"+suffix+".sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob %s/*_%s.sql: matches=%v err=%v", subdir, suffix, matches, err)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	return string(b)
}

// TestCreateForgeMigrationParity proves the migration file schema:
// construct.Create("migration", ...) writes a .sql body byte-identical to
// forge(migration) — canonical AND testutil mirror — with the EXPLAIN
// parse-check and idempotency holding through the umbrella.
func TestCreateForgeMigrationParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	const canonical = "go/internal/db/migrations"
	const mirror = "go/internal/testutil/migrations"
	const upSQL = "CREATE TABLE mig_parity_probe (id INTEGER PRIMARY KEY, note TEXT);"
	const docstring = "Parity probe migration\nsecond docstring line"

	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "migration", "slug": "mig-forge", "up_sql": upSQL, "docstring": docstring,
	})

	deps := construct.Deps{Pool: pool, Schemas: reg}
	if _, err := construct.Create(ctx, deps, "migration", "mcp-servers", construct.Input{
		Migration: &construct.MigrationInput{Slug: "mig-record", UpSQL: upSQL, Docstring: docstring},
	}); err != nil {
		t.Fatalf("Create(migration): %v", err)
	}

	fBody := readMigrationBody(t, root, canonical, "mig-forge")
	rBody := readMigrationBody(t, root, canonical, "mig-record")
	if fBody != rBody {
		t.Fatalf("migration body parity mismatch:\n--- forge ---\n%s\n--- construct ---\n%s", fBody, rBody)
	}
	if rMirror := readMigrationBody(t, root, mirror, "mig-record"); rMirror != rBody {
		t.Fatalf("construct migration mirror != canonical:\n--- canonical ---\n%s\n--- mirror ---\n%s", rBody, rMirror)
	}

	// Parse-check rejection bubbles up through the umbrella.
	if _, err := construct.Create(ctx, deps, "migration", "mcp-servers", construct.Input{
		Migration: &construct.MigrationInput{Slug: "mig-bad", UpSQL: "this is definitely not valid sql"},
	}); err == nil || !strings.Contains(err.Error(), "parse-check") {
		t.Fatalf("expected SQL parse-check rejection, got: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(root, canonical, "*_mig-bad.sql")); len(m) != 0 {
		t.Fatalf("rejected migration should not have written a file, found: %v", m)
	}

	// Idempotency holds through the umbrella.
	res, err := construct.Create(ctx, deps, "migration", "mcp-servers", construct.Input{
		Migration: &construct.MigrationInput{Slug: "mig-record", UpSQL: upSQL, Docstring: docstring},
	})
	if err != nil {
		t.Fatalf("Create(migration) idempotent: %v", err)
	}
	if got := len(res.EventsEmitted); got != 1 {
		t.Fatalf("idempotent Create should still emit 1 event (with idempotent=true), got %d", got)
	}
	var p struct {
		Idempotent bool `json:"idempotent"`
	}
	if err := json.Unmarshal(res.EventsEmitted[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal idempotent payload: %v", err)
	}
	if !p.Idempotent {
		t.Fatalf("re-create of existing slug should be idempotent, payload: %s", res.EventsEmitted[0].Payload)
	}
	if m, _ := filepath.Glob(filepath.Join(root, canonical, "*_mig-record.sql")); len(m) != 1 {
		t.Fatalf("idempotent re-create should not mint a second file, found: %v", m)
	}
}

// ── Slice 3: memory edit parity ────────────────────────────────────────────

// TestUpdateForgeMemoryParity: edit a memory note through forge_edit and
// through construct.Update; the on-disk file bytes must match modulo the
// slug-derived `name:` line (the only legitimate difference, since the
// two memories live at different slugs).
//
// construct.Update additionally emits a MemoryWritten event that refreshes
// proj_memories — forge_edit on memory does NOT emit, so its proj_memories
// row stays at the create-time content. That's a strict improvement, not a
// parity divergence; the test pins both behaviors.
func TestUpdateForgeMemoryParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	reg := loadForgeRegistry(t)
	const kind = "project"
	const origDesc = "original one-liner"
	const origBody = "Original body."

	for _, slug := range []string{"mem-edit-forge", "mem-edit-record"} {
		mustForgeMap(t, pool, "mcp-servers", map[string]any{
			"schema_name": "memory", "slug": slug, "name": slug, "memory_kind": kind,
			"description": origDesc, "body": origBody, "source": "manual",
		})
	}

	// forge_edit path.
	if _, err := forgeEditRaw(t, pool, "mcp-servers", json.RawMessage(
		`{"schema_name":"memory","slug":"mem-edit-forge",
		   "fields":{"description":"updated one-liner","body":"Updated body content."}}`,
	)); err != nil {
		t.Fatalf("forge_edit(memory): %v", err)
	}

	// construct.Update path.
	deps := construct.Deps{Pool: pool, Schemas: reg}
	out, err := construct.Update(ctx, deps, "memory", "mcp-servers", construct.UpdateInput{
		Memory: &construct.MemoryEditInput{
			Slug:        "mem-edit-record",
			Description: strPtr("updated one-liner"),
			Body:        strPtr("Updated body content."),
		},
	})
	if err != nil {
		t.Fatalf("construct.Update(memory): %v", err)
	}
	if out.Relocated {
		t.Fatalf("kind unchanged — should not relocate")
	}
	wantPath := filepath.Join(root, "vault", "memory", kind, "mem-edit-record.md")
	if out.FilePath != wantPath {
		t.Fatalf("UpdateResult.FilePath=%q, want %q", out.FilePath, wantPath)
	}

	// FILE bytes byte-identical (modulo slug-derived name: line).
	fBytes, err := os.ReadFile(filepath.Join(root, "vault", "memory", kind, "mem-edit-forge.md"))
	if err != nil {
		t.Fatalf("read forge file: %v", err)
	}
	rBytes, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read construct file: %v", err)
	}
	fNorm := strings.Replace(string(fBytes), "name: mem-edit-forge", "name: <slug>", 1)
	rNorm := strings.Replace(string(rBytes), "name: mem-edit-record", "name: <slug>", 1)
	if fNorm != rNorm {
		t.Fatalf("memory edit FILE parity mismatch:\n--- forge ---\n%s\n--- construct ---\n%s", fNorm, rNorm)
	}
	if !strings.Contains(rNorm, "description: updated one-liner") || !strings.Contains(rNorm, "Updated body content.") {
		t.Fatalf("construct edit did not apply updates:\n%s", rNorm)
	}

	// proj_memories — post forge-archive (chain 311 T7 Stage 6 P2-C.2) BOTH the
	// forge-shaped edit path (construct.HandleForgeEdit) and construct.Update emit
	// MemoryWritten, so BOTH rows now reflect the new content. Pre-archive the
	// forge_edit path left proj_memories stale (its own quirk); the construct
	// edit's MemoryWritten emit is the strict improvement that's now universal —
	// forge_edit IS the construct path. origDesc is retained as the pre-edit value.
	_ = origDesc
	rKind, rDesc, rLen, _ := readMemoryRow(t, pool, "mem-edit-record")
	if rKind != kind || rDesc != "updated one-liner" || rLen != len("Updated body content.") {
		t.Fatalf("construct.Update should refresh proj_memories (B-ED3 spec): kind=%q desc=%q len=%d", rKind, rDesc, rLen)
	}
	_, fDesc, _, _ := readMemoryRow(t, pool, "mem-edit-forge")
	if fDesc != "updated one-liner" {
		t.Fatalf("forge-shaped memory edit now also refreshes proj_memories (it routes through construct): desc=%q (want %q)", fDesc, "updated one-liner")
	}
}

// TestUpdateMemoryRelocatesOnKindChange: changing memory_kind moves the file
// to the new kind subdir, mirroring forge_edit's relocation behavior.
func TestUpdateMemoryRelocatesOnKindChange(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	reg := loadForgeRegistry(t)
	mustForgeMap(t, pool, "mcp-servers", map[string]any{
		"schema_name": "memory", "slug": "mem-relocate", "name": "mem-relocate",
		"memory_kind": "feedback", "description": "before", "body": "b",
	})

	origPath := filepath.Join(root, "vault", "memory", "feedback", "mem-relocate.md")
	if _, err := os.Stat(origPath); err != nil {
		t.Fatalf("orig file missing: %v", err)
	}

	deps := construct.Deps{Pool: pool, Schemas: reg}
	out, err := construct.Update(ctx, deps, "memory", "mcp-servers", construct.UpdateInput{
		Memory: &construct.MemoryEditInput{Slug: "mem-relocate", Kind: strPtr("project")},
	})
	if err != nil {
		t.Fatalf("construct.Update relocate: %v", err)
	}
	if !out.Relocated {
		t.Fatalf("kind change must set Relocated")
	}
	newPath := filepath.Join(root, "vault", "memory", "project", "mem-relocate.md")
	if out.FilePath != newPath {
		t.Fatalf("UpdateResult.FilePath=%q, want %q", out.FilePath, newPath)
	}
	if _, err := os.Stat(origPath); !os.IsNotExist(err) {
		t.Fatalf("stale file at %q should be removed", origPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new file missing: %v", err)
	}
}

// ── Slice 4: chain-anchored doc (retro + report-card) edit parity ────────

// TestUpdateForgeChainAnchoredDocParity: edit a retrospective or report-card
// through forge_edit and through construct.Update; the file bytes + the
// knowledge_pointer row must match byte-for-byte (the test deliberately
// uses TWO separate temp roots so forge_edit and construct.Update can run
// against equivalent starting docs without colliding on the same file).
func TestUpdateForgeChainAnchoredDocParity(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	reg := loadForgeRegistry(t)

	cases := []struct {
		name      string
		chainSlug string
		sections  map[string]string
		newSec    string
		newVal    string
	}{
		{name: "retrospective", chainSlug: "cad-edit-retro-chain",
			sections: map[string]string{"what_landed": "shipped X", "surprises": "Z was hard"},
			newSec:   "what_landed", newVal: "shipped X AND extras"},
		{name: "report-card", chainSlug: "cad-edit-rc-chain",
			sections: map[string]string{"per_task_grades": "T1: A", "overall_verdict": "SHIP"},
			newSec:   "overall_verdict", newVal: "SHIP with caveats"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chainRaw, _ := json.Marshal(map[string]any{"schema_name": "chain", "slug": tc.chainSlug,
				"output": "o", "design_decisions": "dd", "completion_condition": "cc"})
			if r, err := forgeCreateRaw(t, pool, "mcp-servers", chainRaw); err != nil || r.Error != "" {
				t.Fatalf("forge(chain): %v %q", err, r.Error)
			}

			// forge_edit path: separate root.
			rootF := t.TempDir()
			t.Setenv("FORGE_MARKDOWN_ROOT", rootF)
			fparams := map[string]any{"schema_name": tc.name, "slug": "edit-doc-" + tc.name, "chain_slug": tc.chainSlug}
			for k, v := range tc.sections {
				fparams[k] = v
			}
			fraw, _ := json.Marshal(fparams)
			if r, err := forgeCreateRaw(t, pool, "mcp-servers", fraw); err != nil || r.Error != "" {
				t.Fatalf("forge(%s) create: %v %q", tc.name, err, r.Error)
			}
			editRaw, _ := json.Marshal(map[string]any{
				"schema_name": tc.name, "slug": "edit-doc-" + tc.name,
				"fields": map[string]any{tc.newSec: tc.newVal},
			})
			if r, err := forgeEditRaw(t, pool, "mcp-servers", editRaw); err != nil || r.Error != "" {
				t.Fatalf("forge_edit(%s): %v %q", tc.name, err, r.Error)
			}
			fFile := readOnlyDoc(t, rootF)
			fq, fd, _, _, _ := readOnlyPointerFields(t, pool, tc.name)

			// construct.Update path: separate root.
			rootR := t.TempDir()
			t.Setenv("FORGE_MARKDOWN_ROOT", rootR)
			constructDeps := construct.Deps{Pool: pool, Schemas: reg}
			var in construct.Input
			doc := &construct.ChainAnchoredDocInput{Slug: "edit-doc-" + tc.name, ChainSlug: tc.chainSlug, Sections: tc.sections}
			if tc.name == "retrospective" {
				in.Retrospective = doc
			} else {
				in.ReportCard = doc
			}
			if _, err := construct.Create(ctx, constructDeps, tc.name, "mcp-servers", in); err != nil {
				t.Fatalf("Create(%s): %v", tc.name, err)
			}
			editInput := construct.UpdateInput{}
			editPartial := &construct.ChainAnchoredDocEditInput{
				Slug:     "edit-doc-" + tc.name,
				Sections: map[string]*string{tc.newSec: strPtr(tc.newVal)},
			}
			if tc.name == "retrospective" {
				editInput.Retrospective = editPartial
			} else {
				editInput.ReportCard = editPartial
			}
			out, err := construct.Update(ctx, constructDeps, tc.name, "mcp-servers", editInput)
			if err != nil {
				t.Fatalf("construct.Update(%s): %v", tc.name, err)
			}
			if out.FilePath == "" {
				t.Fatalf("UpdateResult.FilePath empty for %s", tc.name)
			}

			rFile := readOnlyDoc(t, rootR)
			if fFile != rFile {
				t.Fatalf("%s edit FILE parity mismatch:\n--- forge ---\n%s\n--- construct ---\n%s", tc.name, fFile, rFile)
			}
			rq, rd, _, _, _ := readOnlyPointerFields(t, pool, tc.name)
			if fq != rq || fd != rd {
				t.Fatalf("%s edit pointer parity mismatch:\n  forge:     q=%q d=%q\n  construct: q=%q d=%q",
					tc.name, fq, fd, rq, rd)
			}
		})
	}
}

// TestUpdateChainAnchoredDocNotFound: editing an unknown slug surfaces the
// seam's NotFound as a clear error.
func TestUpdateChainAnchoredDocNotFound(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	_, err := construct.Update(ctx, deps, "retrospective", "mcp-servers", construct.UpdateInput{
		Retrospective: &construct.ChainAnchoredDocEditInput{
			Slug:     "no-such-retro",
			Sections: map[string]*string{"what_landed": strPtr("x")},
		},
	})
	if err == nil {
		t.Fatalf("expected retro not-found, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found phrasing: %v", err)
	}
}

// TestUpdateMemoryNotFound: editing an unknown memory slug returns the
// seam's NotFound surfaced as a clear error.
func TestUpdateMemoryNotFound(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FORGE_MARKDOWN_ROOT", root)

	deps := construct.Deps{Pool: pool, Schemas: loadForgeRegistry(t)}
	_, err := construct.Update(ctx, deps, "memory", "mcp-servers", construct.UpdateInput{
		Memory: &construct.MemoryEditInput{Slug: "no-such-memory", Description: strPtr("x")},
	})
	if err == nil {
		t.Fatalf("expected memory not-found, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found phrasing: %v", err)
	}
}
