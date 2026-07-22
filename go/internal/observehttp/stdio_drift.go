package observehttp

import (
	"net/http"

	"toolkit/internal/stdiodrift"
)

// stdioDriftState is the GET /admin/stdio-drift-state handler. It
// surfaces the post-commit-restart-advisor's marker plus per-PID
// fd-deletion state so dashboards (and parse_context, via the shared
// stdiodrift package) can detect a stdio toolkit-server running
// against an older binary than HEAD.
//
// Response shape mirrors stdiodrift.State.
//
// Failure modes:
//   - Marker absent → 200 with drift_detected=false, stdio_processes=[].
//     Dominant happy path; not an error.
//   - Marker read error (permission denied, etc.) → 500.
//   - git rev-parse HEAD unreachable → 200 with head_sha="" and any
//     marker-derived drift signals still surfaced via fd_deleted.
//
// Chain parse-context-lean-orienting T9.
func (s AppState) stdioDriftState(w http.ResponseWriter, r *http.Request) {
	state, err := stdiodrift.Snapshot(r.Context(), stdiodrift.SnapshotInputs{
		RepoRoot:           s.RepoRoot,
		OnDiskGitSHA:       s.GitSHA,
		MarkerPathOverride: s.DriftMarkerPathOverride,
		ProcRootOverride:   s.DriftProcRootOverride,
	})
	if err != nil {
		dbErr(w, err)
		return
	}
	// Don't cache — agents and dashboards expect fresh state on every
	// poll. The post-commit advisor's marker mutates between reads.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, state)
}
