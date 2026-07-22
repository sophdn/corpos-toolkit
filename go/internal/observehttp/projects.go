package observehttp

import (
	"net/http"
)

// projectRow mirrors the dashboard's ProjectInfo interface
// (apps/dashboard/src/api/admin.ts). The picker uses `id` as the
// select-option value and `name` as its label.
type projectRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

// projectsList returns every registered project, ordered by name. Drives
// the dashboard's ProjectPicker dropdown (now living on the Roadmap
// page). Read-only; cross-project by design — the picker itself is what
// scopes downstream queries.
func (s AppState) projectsList(w http.ResponseWriter, r *http.Request) {
	const q = `SELECT id, name, path, created_at FROM projects ORDER BY name ASC`
	rows, err := s.Pool.DB().QueryContext(r.Context(), q)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []projectRow{}
	for rows.Next() {
		var p projectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.CreatedAt); err != nil {
			dbErr(w, err)
			return
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
