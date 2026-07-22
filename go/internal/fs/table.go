package fs

import (
	"context"
	"encoding/json"

	"toolkit/internal/dispatch"
)

// BuildTable returns the fs surface's dispatch.Table. read/write/edit share the
// Deps.Reads registry (read records a full read; write/edit require one on an
// existing file); Pool is captured for the opt-in substrate-native upgrade
// modes. Pairs with the BuildTable functions in internal/work, internal/knowledge,
// internal/measure, internal/admin, internal/ml.
func BuildTable(deps Deps) dispatch.Table {
	if deps.Reads == nil {
		deps.Reads = NewReadRegistry()
	}
	reg := deps.Reads
	return dispatch.Table{
		"read": dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (ReadResult, error) {
			// Opt-in substrate-native modes route to the upgrade dispatcher; the
			// byte-parity default stays the original path (and is the only path
			// that records full read-state — a mode read is a partial/derived
			// view that must not satisfy the write/edit precondition).
			if readModeOf(params) != modeNone {
				return handleReadMode(ctx, deps, params)
			}
			res, err := HandleRead(ctx, params)
			if err == nil {
				noteRead(reg, params)
			}
			return res, err
		}),
		"write": dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (WriteResult, error) {
			res, err := HandleWrite(ctx, params, reg)
			// OPT-IN provenance stamping, layered after the parity write so the
			// default path is untouched. Fail-open: an emit error never fails a
			// write that already committed.
			if err == nil {
				var p WriteParams
				if json.Unmarshal(params, &p) == nil {
					res.Event = maybeEmitWriteArtifact(ctx, deps.Pool, p, res)
				}
			}
			return res, err
		}),
		"edit": dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (EditResult, error) {
			res, err := HandleEdit(ctx, params, reg)
			// OPT-IN provenance stamping, layered after the parity edit so the
			// default path is untouched. Fail-open: an emit error never fails an
			// edit that already committed.
			if err == nil {
				var p EditParams
				if json.Unmarshal(params, &p) == nil {
					res.Event = maybeEmitEditArtifact(ctx, deps.Pool, p, res)
				}
			}
			return res, err
		}),
		// Mutating relocation. Not read-state coupled (relocates bytes, does not
		// rewrite content). OPT-IN provenance stamping layered after the move.
		"move": dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (MoveResult, error) {
			res, err := HandleMove(ctx, params)
			if err == nil {
				var p MoveParams
				if json.Unmarshal(params, &p) == nil {
					res.Event = maybeEmitMoveArtifact(ctx, deps.Pool, p, res)
				}
			}
			return res, err
		}),
		// Destructive deletion. Rationale-gated at the dispatch boundary for agent
		// actors; OPT-IN provenance stamping layered after the remove.
		"remove": dispatch.AdaptParamsOnly(func(ctx context.Context, params json.RawMessage) (RemoveResult, error) {
			res, err := HandleRemove(ctx, params)
			if err == nil {
				var p RemoveParams
				if json.Unmarshal(params, &p) == nil {
					res.Event = maybeEmitRemoveArtifact(ctx, deps.Pool, p, res)
				}
			}
			return res, err
		}),
		// Read-only search/listing actions — no read-state coupling, no DB.
		"grep": dispatch.AdaptParamsOnly(HandleGrep),
		"glob": dispatch.AdaptParamsOnly(HandleGlob),
		"ls":   dispatch.AdaptParamsOnly(HandleLS),
	}
}
