package observehttp

import (
	"net/http"
)

// roadmapEntry mirrors work_lib::roadmap::RoadmapEntry. The four
// nullable columns (chain_slug, note, status, updated_at) surface as
// *string so the JSON encoder emits explicit null — the dashboard's
// TypeScript RoadmapEntry types these as `string | null`.
type roadmapEntry struct {
	Position  int64   `json:"position"`
	ProjectID string  `json:"project_id"`
	RefKind   string  `json:"ref_kind"`
	RefSlug   string  `json:"ref_slug"`
	ChainSlug *string `json:"chain_slug"`
	Note      *string `json:"note"`
	Status    *string `json:"status"`
	UpdatedAt *string `json:"updated_at"`
}

// roadmapUnplacedRef mirrors work_lib::roadmap::UnplacedRef.
type roadmapUnplacedRef struct {
	Slug      string  `json:"slug"`
	ProjectID string  `json:"project_id"`
	CreatedAt string  `json:"created_at"`
	ChainSlug *string `json:"chain_slug"`
}

type roadmapDiffResponse struct {
	Chains []roadmapUnplacedRef `json:"chains"`
	Tasks  []roadmapUnplacedRef `json:"tasks"`
}

func (s AppState) roadmapList(w http.ResponseWriter, r *http.Request) {
	// Read path repointed to proj_roadmap_view (T4 of
	// agent-first-substrate chain). target_status / target_updated_at
	// are denormalised by every Chain*/Task* event fold; the two
	// LEFT JOINs the legacy query carried are pre-evaluated at write
	// time. Roadmap layout columns still come from roadmap_items via
	// the projection's snapshot (roadmap_set / roadmap_insert remain
	// non-emitting in this chain).
	const q = `
SELECT position, project_id, ref_kind, ref_slug, chain_slug, note,
       target_status AS status, target_updated_at AS updated_at
FROM proj_roadmap_view
ORDER BY position ASC`

	rows, err := s.Pool.DB().QueryContext(r.Context(), q)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()

	out := []roadmapEntry{}
	for rows.Next() {
		var e roadmapEntry
		if err := rows.Scan(&e.Position, &e.ProjectID, &e.RefKind, &e.RefSlug,
			&e.ChainSlug, &e.Note, &e.Status, &e.UpdatedAt); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s AppState) roadmapDiff(w http.ResponseWriter, r *http.Request) {
	var lastReassessed string
	if err := s.Pool.DB().QueryRowContext(r.Context(),
		`SELECT value FROM roadmap_meta WHERE key = 'last_reassessed_at'`,
	).Scan(&lastReassessed); err != nil {
		dbErr(w, err)
		return
	}

	chains, err := roadmapUnplaced(r, s,
		`SELECT slug, project_id, created_at, NULL AS chain_slug FROM proj_chain_status
		 WHERE status = 'open'
		   AND created_at > ?
		   AND slug NOT IN (SELECT ref_slug FROM proj_roadmap_view WHERE ref_kind = 'chain')
		 ORDER BY created_at ASC`,
		lastReassessed)
	if err != nil {
		dbErr(w, err)
		return
	}
	tasks, err := roadmapUnplaced(r, s,
		`SELECT t.slug, c.project_id, t.created_at, c.slug AS chain_slug
		 FROM proj_current_tasks t JOIN proj_chain_status c ON t.chain_id = c.id
		 WHERE t.status IN ('pending', 'active')
		   AND t.created_at > ?
		   AND t.slug NOT IN (SELECT ref_slug FROM proj_roadmap_view WHERE ref_kind = 'task')
		 ORDER BY t.created_at ASC`,
		lastReassessed)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roadmapDiffResponse{Chains: chains, Tasks: tasks})
}

func roadmapUnplaced(r *http.Request, s AppState, q, since string) ([]roadmapUnplacedRef, error) {
	rows, err := s.Pool.DB().QueryContext(r.Context(), q, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []roadmapUnplacedRef{}
	for rows.Next() {
		var u roadmapUnplacedRef
		if err := rows.Scan(&u.Slug, &u.ProjectID, &u.CreatedAt, &u.ChainSlug); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
