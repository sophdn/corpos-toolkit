package observehttp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"toolkit/internal/db"
)

// study_runs.go serves the read side of the study-run feature (corpos-lab
// behavioral assays persisted via the measure study_run_record action). Two
// endpoints, both reading the projection pair populated by the
// StudyRunRecorded fold (go/internal/projections/study_runs.go):
//
//   - GET /study-runs            → studyRunsList  (parent rows, filterable)
//   - GET /study-runs/{run_id}   → studyRunDetail (parent + score grid)
//
// Both return bare JSON (an array / an object) with json-tagged shapes so the
// dashboard + corpos-lab client bind directly. Ordering is run_at DESC
// (never event_id — gate-banned; run_at is RFC 3339 UTC so lexical ==
// chronological).

// studyRunRow is one parent row of proj_study_runs. materials_hashes is
// re-hydrated from the stored JSON-object text so the client sees a map, not
// a string.
type studyRunRow struct {
	ID              string            `json:"run_id"`
	ProjectID       string            `json:"project_id"`
	Name            string            `json:"name"`
	Assay           string            `json:"assay"`
	ItemID          string            `json:"item_id"`
	ImageRef        string            `json:"image_ref"`
	ImageDigest     string            `json:"image_digest"`
	StudyDigest     string            `json:"study_digest"`
	MaterialsHashes map[string]string `json:"materials_hashes"`
	ModelID         string            `json:"model_id"`
	ModelVersion    string            `json:"model_version"`
	Status          string            `json:"status"`
	Error           string            `json:"error"`
	ResponsesDir    string            `json:"responses_dir"`
	RunAt           string            `json:"run_at"`
}

// studyRunScore is one child row of proj_study_run_scores (run_idx serialised
// as "run" to match the record corpos-lab sent).
type studyRunScore struct {
	Condition     string `json:"condition"`
	Run           int64  `json:"run"`
	VerdictKind   string `json:"verdict_kind"`
	VerdictReason string `json:"verdict_reason"`
	Item          string `json:"item"`
	Rationale     string `json:"rationale"`
}

// studyRunDetailResponse is the parent row plus its score grid.
type studyRunDetailResponse struct {
	studyRunRow
	Scores []studyRunScore `json:"scores"`
}

const studyRunParentColumns = `id, project_id, name, assay, item_id, image_ref,
	image_digest, study_digest, materials_hash_json, model_id, model_version,
	status, error, responses_dir, run_at`

// hydrateMaterials re-hydrates the materials_hash_json column into a map. A
// parse failure degrades to an empty map rather than failing the whole
// response (the column is a denormalised cache of small hashes, not
// load-bearing for the row's identity). Always returns a non-nil map so the
// JSON output is an object, never null.
func hydrateMaterials(materialsJSON string) map[string]string {
	m := map[string]string{}
	if materialsJSON != "" {
		_ = json.Unmarshal([]byte(materialsJSON), &m)
	}
	return m
}

// studyRunsList serves GET /study-runs. Filters: project, assay, model_id,
// status, run_id, since (RFC 3339 lower bound on run_at), limit (clamped
// 1..5000, default 500). Ordered run_at DESC.
func (s AppState) studyRunsList(w http.ResponseWriter, r *http.Request) {
	project := projectFilter(r)
	assay := r.URL.Query().Get("assay")
	modelID := r.URL.Query().Get("model_id")
	status := r.URL.Query().Get("status")
	runID := r.URL.Query().Get("run_id")
	// since is an RFC 3339 lower bound compared lexically against run_at
	// (run_at is a UTC RFC 3339 string, not unix seconds — so the shared
	// int64 optSince helper doesn't apply here).
	since := r.URL.Query().Get("since")
	limit := int64(500)
	if v := r.URL.Query().Get("limit"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 5000 {
		limit = 5000
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(studyRunParentColumns)
	b.WriteString(" FROM proj_study_runs WHERE 1=1")
	binds := db.NewArgs()
	if project != "" {
		b.WriteString(" AND project_id = ?")
		binds.AddString(project)
	}
	if assay != "" {
		b.WriteString(" AND assay = ?")
		binds.AddString(assay)
	}
	if modelID != "" {
		b.WriteString(" AND model_id = ?")
		binds.AddString(modelID)
	}
	if status != "" {
		b.WriteString(" AND status = ?")
		binds.AddString(status)
	}
	if runID != "" {
		b.WriteString(" AND id = ?")
		binds.AddString(runID)
	}
	if since != "" {
		b.WriteString(" AND run_at >= ?")
		binds.AddString(since)
	}
	b.WriteString(fmt.Sprintf(" ORDER BY run_at DESC LIMIT %d", limit))

	rows, err := s.Pool.DB().QueryContext(r.Context(), b.String(), binds.Slice()...)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer rows.Close()
	out := []studyRunRow{}
	for rows.Next() {
		var row studyRunRow
		var materialsJSON string
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.Name, &row.Assay, &row.ItemID,
			&row.ImageRef, &row.ImageDigest, &row.StudyDigest, &materialsJSON,
			&row.ModelID, &row.ModelVersion, &row.Status, &row.Error,
			&row.ResponsesDir, &row.RunAt); err != nil {
			dbErr(w, err)
			return
		}
		row.MaterialsHashes = hydrateMaterials(materialsJSON)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// studyRunDetail serves GET /study-runs/{run_id}: the parent row plus its
// proj_study_run_scores grid (ordered condition, run_idx for a stable grid).
func (s AppState) studyRunDetail(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("run_id")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "run_id is required"})
		return
	}

	var row studyRunRow
	var materialsJSON string
	if err := s.Pool.DB().QueryRowContext(r.Context(),
		"SELECT "+studyRunParentColumns+" FROM proj_study_runs WHERE id = ?", runID).
		Scan(&row.ID, &row.ProjectID, &row.Name, &row.Assay, &row.ItemID,
			&row.ImageRef, &row.ImageDigest, &row.StudyDigest, &materialsJSON,
			&row.ModelID, &row.ModelVersion, &row.Status, &row.Error,
			&row.ResponsesDir, &row.RunAt); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "study run not found"})
			return
		}
		dbErr(w, err)
		return
	}
	row.MaterialsHashes = hydrateMaterials(materialsJSON)

	scoreRows, err := s.Pool.DB().QueryContext(r.Context(),
		`SELECT condition, run_idx, verdict_kind, verdict_reason, item, rationale
		 FROM proj_study_run_scores WHERE run_id = ?
		 ORDER BY condition, run_idx`, runID)
	if err != nil {
		dbErr(w, err)
		return
	}
	defer scoreRows.Close()
	scores := []studyRunScore{}
	for scoreRows.Next() {
		var sr studyRunScore
		if err := scoreRows.Scan(&sr.Condition, &sr.Run, &sr.VerdictKind,
			&sr.VerdictReason, &sr.Item, &sr.Rationale); err != nil {
			dbErr(w, err)
			return
		}
		scores = append(scores, sr)
	}
	if err := scoreRows.Err(); err != nil {
		dbErr(w, err)
		return
	}

	writeJSON(w, http.StatusOK, studyRunDetailResponse{studyRunRow: row, Scores: scores})
}
