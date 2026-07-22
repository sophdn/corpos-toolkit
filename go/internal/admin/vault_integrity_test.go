package admin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/knowledge/vault"
)

// setVaultRoot points TOOLKIT_VAULT_ROOT at a t.TempDir for the
// duration of the test. ResolveRoot's first-precedence override is
// the env var; pinning it keeps the test hermetic against the real
// $HOME/.claude/vault.
func setVaultRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv(vault.VaultRootEnv, root)
	return root
}

func seedAdminVaultPointer(t *testing.T, d Deps, slug, sourceRef string) int64 {
	t.Helper()
	res, err := pointers.Upsert(context.Background(), d.Pool, pointers.KnowledgePointer{
		ProjectID:  "vault",
		SourceType: "vault",
		SourceRef:  sourceRef,
		Slug:       slug,
		Question:   "Q for " + slug,
		InvokeWhen: "when " + slug,
		Tags:       []string{},
	})
	if err != nil {
		t.Fatalf("seed pointer %s: %v", slug, err)
	}
	return res.ID
}

func writeVaultFile(t *testing.T, root, relative string) {
	t.Helper()
	abs := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", relative, err)
	}
	if err := os.WriteFile(abs, []byte("ok"), 0o644); err != nil {
		t.Fatalf("write %s: %v", relative, err)
	}
}

func TestVaultOrphanList_DryRunDoesNotMutate(t *testing.T) {
	d := mkDeps(t)
	root := setVaultRoot(t)

	presentID := seedAdminVaultPointer(t, d, "have-file", "decisions/2026-05-20_have.md")
	orphanID := seedAdminVaultPointer(t, d, "no-file", "decisions/2026-05-20_missing.md")
	writeVaultFile(t, root, "decisions/2026-05-20_have.md")

	got, err := d.vaultOrphanList(context.Background())
	if err != nil {
		t.Fatalf("vaultOrphanList: %v", err)
	}
	if got.VaultRoot == "" {
		t.Error("vault_root should be populated in the response")
	}
	if got.TotalPointers != 2 {
		t.Errorf("total_pointers = %d; want 2", got.TotalPointers)
	}
	if got.FilesPresent != 1 {
		t.Errorf("files_present = %d; want 1", got.FilesPresent)
	}
	if got.OrphansFound != 1 {
		t.Errorf("orphans_found = %d; want 1", got.OrphansFound)
	}
	if len(got.Orphans) != 1 || got.Orphans[0].ID != orphanID {
		t.Errorf("orphans = %+v; want [{id=%d}]", got.Orphans, orphanID)
	}

	// Dry-run promise: status of the orphan must NOT have flipped.
	var status string
	if err := d.Pool.DB().QueryRowContext(context.Background(),
		`SELECT status FROM knowledge_pointers WHERE id = ?`, orphanID).Scan(&status); err != nil {
		t.Fatalf("post-list SELECT: %v", err)
	}
	if status != "active" {
		t.Errorf("orphan status after dry-run = %q; want active (list must not mutate)", status)
	}
	if status := pointerStatus(t, d, presentID); status != "active" {
		t.Errorf("present pointer status = %q; want active", status)
	}
}

func TestVaultIntegritySweep_RetiresOrphans(t *testing.T) {
	d := mkDeps(t)
	root := setVaultRoot(t)

	presentID := seedAdminVaultPointer(t, d, "have-file", "reference/present.md")
	orphanID := seedAdminVaultPointer(t, d, "no-file", "reference/missing.md")
	writeVaultFile(t, root, "reference/present.md")

	got, err := d.vaultIntegritySweep(context.Background())
	if err != nil {
		t.Fatalf("vaultIntegritySweep: %v", err)
	}
	if got.OrphansFound != 1 {
		t.Errorf("orphans_found = %d; want 1", got.OrphansFound)
	}
	if got.OrphansRetired != 1 {
		t.Errorf("orphans_retired = %d; want 1", got.OrphansRetired)
	}
	if got.TotalPointers != 2 {
		t.Errorf("total_pointers = %d; want 2", got.TotalPointers)
	}

	if status := pointerStatus(t, d, orphanID); status != "orphaned" {
		t.Errorf("orphan post-sweep status = %q; want orphaned", status)
	}
	if status := pointerStatus(t, d, presentID); status != "active" {
		t.Errorf("present pointer post-sweep status = %q; want active (false-positive retirement)", status)
	}

	// Re-running the sweep is a no-op now that the orphan is retired.
	again, err := d.vaultIntegritySweep(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.OrphansFound != 0 || again.OrphansRetired != 0 {
		t.Errorf("second sweep found/retired = %d/%d; want 0/0 (already-orphaned rows excluded)",
			again.OrphansFound, again.OrphansRetired)
	}
}

func TestVaultIntegritySweep_NoOrphansLeavesEverythingActive(t *testing.T) {
	d := mkDeps(t)
	root := setVaultRoot(t)

	id := seedAdminVaultPointer(t, d, "everything-fine", "decisions/fine.md")
	writeVaultFile(t, root, "decisions/fine.md")

	got, err := d.vaultIntegritySweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got.OrphansFound != 0 || got.OrphansRetired != 0 {
		t.Errorf("clean vault sweep found/retired = %d/%d; want 0/0",
			got.OrphansFound, got.OrphansRetired)
	}
	if status := pointerStatus(t, d, id); status != "active" {
		t.Errorf("clean-vault pointer status = %q; want active", status)
	}
}

func pointerStatus(t *testing.T, d Deps, id int64) string {
	t.Helper()
	var s string
	if err := d.Pool.DB().QueryRowContext(context.Background(),
		`SELECT status FROM knowledge_pointers WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("status SELECT for id %d: %v", id, err)
	}
	return s
}
