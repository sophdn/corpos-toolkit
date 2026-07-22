package admin

import (
	"context"
	"errors"
	"log/slog"

	"toolkit/internal/knowledge/pointers"
	"toolkit/internal/knowledge/vault"
	"toolkit/internal/obs"
)

// VaultOrphanListResult is the response shape for
// admin.vault_orphan_list — a read-only dry-run that reports every
// active vault pointer whose source_ref file is missing on disk.
// Mirrors the field set on VaultIntegritySweepResult so dashboards
// can render both responses with the same code path.
type VaultOrphanListResult struct {
	VaultRoot     string               `json:"vault_root"`
	TotalPointers int                  `json:"total_pointers"`
	FilesPresent  int                  `json:"files_present"`
	OrphansFound  int                  `json:"orphans_found"`
	Orphans       []pointers.OrphanRow `json:"orphans"`
}

// VaultIntegritySweepResult is the response shape for
// admin.vault_integrity_sweep — the same dry-run scan plus the
// committed retirement count. OrphansRetired may differ from
// OrphansFound when a concurrent caller has already retired a row
// between scan and write (ErrNotFound is treated as a benign race).
type VaultIntegritySweepResult struct {
	VaultRoot      string               `json:"vault_root"`
	TotalPointers  int                  `json:"total_pointers"`
	FilesPresent   int                  `json:"files_present"`
	OrphansFound   int                  `json:"orphans_found"`
	OrphansRetired int                  `json:"orphans_retired"`
	Orphans        []pointers.OrphanRow `json:"orphans"`
}

func (d Deps) vaultOrphanList(ctx context.Context) (VaultOrphanListResult, error) {
	root, err := vault.ResolveRoot("")
	if err != nil {
		return VaultOrphanListResult{}, err
	}
	report, err := pointers.ListVaultOrphans(ctx, d.Pool, root)
	if err != nil {
		return VaultOrphanListResult{}, err
	}
	return VaultOrphanListResult{
		VaultRoot:     root,
		TotalPointers: report.TotalPointers,
		FilesPresent:  report.FilesPresent,
		OrphansFound:  report.OrphansFound,
		Orphans:       report.Orphans,
	}, nil
}

func (d Deps) vaultIntegritySweep(ctx context.Context) (VaultIntegritySweepResult, error) {
	root, err := vault.ResolveRoot("")
	if err != nil {
		return VaultIntegritySweepResult{}, err
	}
	report, err := pointers.ListVaultOrphans(ctx, d.Pool, root)
	if err != nil {
		return VaultIntegritySweepResult{}, err
	}
	retired := 0
	for _, o := range report.Orphans {
		if err := pointers.RetireOrphan(ctx, d.Pool, o.ID); err != nil {
			if errors.Is(err, pointers.ErrNotFound) {
				continue
			}
			return VaultIntegritySweepResult{}, err
		}
		retired++
	}
	return VaultIntegritySweepResult{
		VaultRoot:      root,
		TotalPointers:  report.TotalPointers,
		FilesPresent:   report.FilesPresent,
		OrphansFound:   report.OrphansFound,
		OrphansRetired: retired,
		Orphans:        report.Orphans,
	}, nil
}

// RunStartupVaultIntegritySweep is the optional background hook
// callers wire from main.go after admin.BuildTable. It runs the
// retire-orphans pass in a goroutine so a slow filesystem cannot
// stall server bootstrap. Logged via obs.L() at info on success and
// warn on failure; never panics, never returns an error to the
// caller (fire-and-forget).
//
// Tests skip the hook by not calling it; production wires it
// unconditionally — the hook is cheap (one query + one stat() per
// active vault pointer; chain 600 T9 zeroed the current orphan
// count, so steady-state work is just counts).
func (d Deps) RunStartupVaultIntegritySweep(ctx context.Context) {
	go func() {
		report, err := d.vaultIntegritySweep(ctx)
		if err != nil {
			obs.L().Warn("vault integrity sweep on startup failed",
				slog.String("err", err.Error()))
			return
		}
		obs.L().Info("vault integrity sweep on startup",
			slog.String("vault_root", report.VaultRoot),
			slog.Int("total_pointers", report.TotalPointers),
			slog.Int("files_present", report.FilesPresent),
			slog.Int("orphans_found", report.OrphansFound),
			slog.Int("orphans_retired", report.OrphansRetired),
		)
	}()
}
