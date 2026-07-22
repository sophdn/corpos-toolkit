package work

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"toolkit/internal/db"
)

// TrainedModel mirrors the trained_models table row. The row's lifecycle
// drives the go/internal/ml/ registry (T3): only rows in status='ab_testing'
// or 'promoted' are loadable for live traffic. See
// docs/ML_CAPABILITY_SUBSTRATE.md §4 for the schema + state machine.
type TrainedModel struct {
	ID                       int64  `json:"id"`
	ProjectID                string `json:"project_id"`
	Slug                     string `json:"slug"`
	Task                     string `json:"task"`
	Version                  string `json:"version"`
	TrainingDatasetSignature string `json:"training_dataset_signature"`
	EvalMetrics              string `json:"eval_metrics"`
	Status                   string `json:"status"`
	ArtifactPath             string `json:"artifact_path"`
	CreatedAt                string `json:"created_at"`
	UpdatedAt                string `json:"updated_at"`
}

// TrainedModelListItem is the compact projection trained_model_list
// returns by default. Drops eval_metrics body and the dataset signature
// for scan-density; callers that want them follow up with
// forge_edit/forge_schema introspection or read the full row.
type TrainedModelListItem struct {
	ID           int64  `json:"id"`
	ProjectID    string `json:"project_id"`
	Slug         string `json:"slug"`
	Task         string `json:"task"`
	Version      string `json:"version"`
	Status       string `json:"status"`
	ArtifactPath string `json:"artifact_path"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// validTrainedModelStatus enumerates the lifecycle states the CHECK
// constraint enforces. Mirrored at the action-handler seam so callers
// get a typed error rather than a CHECK violation. See migration 043.
var validTrainedModelStatus = map[string]struct{}{
	"training":   {},
	"evaluating": {},
	"ab_testing": {},
	"promoted":   {},
	"retired":    {},
}

// ErrTrainedModelNotFound surfaces from list/promote/retire when no row
// matches the supplied slug under the resolved project scope. Lets the
// dispatch layer return a typed envelope rather than a generic SQL
// no-rows error.
var ErrTrainedModelNotFound = errors.New("trained_model_not_found")

// trainedModelListParams captures the filter set trained_model_list
// accepts. All fields are optional; empty omits the predicate.
type trainedModelListParams struct {
	Task    string `json:"task"`
	Status  string `json:"status"`
	Verbose bool   `json:"verbose"`
	Limit   int64  `json:"limit"`
	Offset  int64  `json:"offset"`
}

// TrainedModelListResult is the result type for trained_model_list. The
// shape mirrors BugListResult: a verbose path returns full rows, the
// default path returns the compact projection.
type TrainedModelListResult struct {
	Verbose      bool                   `json:"verbose,omitempty"`
	VerboseItems []TrainedModel         `json:"verbose_items,omitempty"`
	DefaultItems []TrainedModelListItem `json:"items,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// HandleTrainedModelList implements work.trained_model_list. Scope
// resolution falls back to cross-project when project is empty
// (mirrors bug_list / roadmap_list). Bounded by the default limit of
// 50 so the response stays small even on a full-table query.
func HandleTrainedModelList(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TrainedModelListResult, error) {
	var p trainedModelListParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TrainedModelListResult{}, fmt.Errorf("parse params: %w", err)
		}
	}

	limit, offset := normalizeLimitOffset(p.Limit, p.Offset, 50)
	args := db.NewArgs()
	var conds []string
	if project != "" {
		conds = append(conds, "project_id = ?")
		args.AddString(project)
	}
	if p.Task != "" {
		conds = append(conds, "task = ?")
		args.AddString(p.Task)
	}
	if p.Status != "" {
		if _, ok := validTrainedModelStatus[p.Status]; !ok {
			return TrainedModelListResult{
				Error: fmt.Sprintf("invalid status filter %q (accepted: training, evaluating, ab_testing, promoted, retired)", p.Status),
			}, nil
		}
		conds = append(conds, "status = ?")
		args.AddString(p.Status)
	}

	whereClause := ""
	if len(conds) > 0 {
		whereClause = "WHERE " + strings.Join(conds, " AND ")
	}
	limitClause := ""
	if limit >= 0 {
		if offset > 0 {
			limitClause = fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)
		} else {
			limitClause = fmt.Sprintf("LIMIT %d", limit)
		}
	}

	if p.Verbose {
		query := fmt.Sprintf(`SELECT id, project_id, slug, task, version,
			training_dataset_signature, eval_metrics, status, artifact_path,
			created_at, updated_at
			FROM trained_models %s
			ORDER BY task ASC, version DESC, created_at DESC %s`, whereClause, limitClause)
		rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
		if err != nil {
			return TrainedModelListResult{}, err
		}
		defer rows.Close()
		out, err := scanTrainedModels(rows)
		if err != nil {
			return TrainedModelListResult{}, err
		}
		return TrainedModelListResult{Verbose: true, VerboseItems: out}, nil
	}

	query := fmt.Sprintf(`SELECT id, project_id, slug, task, version, status,
		artifact_path, created_at, updated_at
		FROM trained_models %s
		ORDER BY task ASC, version DESC, created_at DESC %s`, whereClause, limitClause)
	rows, err := pool.DB().QueryContext(ctx, query, args.Slice()...)
	if err != nil {
		return TrainedModelListResult{}, err
	}
	defer rows.Close()
	items, err := scanTrainedModelListItems(rows)
	if err != nil {
		return TrainedModelListResult{}, err
	}
	return TrainedModelListResult{DefaultItems: items}, nil
}

// HandleTrainedModelCreate inserts a new trained_models row via a direct
// (project_id, slug)-keyed INSERT, parallel to HandleTrainedModelPromote /
// HandleTrainedModelRetire. It is the chain 311 T7 Stage 6 P2-C.1 "minimal
// sever": trained_model create routes here instead of forge.GenericStrategy so
// trained_models becomes plain work/-owned CRUD (create + promote + retire all
// direct, no events) and forge can archive. Full event-sourcing (a
// TrainedModelForged event + projection fold) is deferred to chain
// trained-model-event-source-migration — sourcing only the create while
// promote/retire stay direct UPDATEs would be an incoherent projection.
//
// MIRRORS forge.GenericStrategy.Create's column set exactly: project_id + slug +
// the declared fields (task, version, training_dataset_signature, eval_metrics,
// artifact_path), plus status. status defaults to 'training' when empty — the
// same value migration 043's column DEFAULT supplies, so the stored row is
// byte-identical whether the default lands here or at the DB. A duplicate
// (project_id, slug) surfaces as the UNIQUE-constraint error the bare INSERT
// raises (GenericStrategy has no once-only dup envelope for generic shapes; the
// event-sourced shapes' RejectDuplicateBySlug guard does not apply here).
//
// The agent-facing front (forge.PrepareForge validate/slug) + tail
// (FinalizeForgeCreate: SSE publish, response envelope) are unchanged — the
// dispatch adapter (cmd/toolkit-server.handleAgentForge) extracts the validated
// fields and runs that tail; only the persistence layer moved off forge.
func HandleTrainedModelCreate(ctx context.Context, pool *db.Pool, project, slug, task, version, trainingDatasetSignature, evalMetrics, status, artifactPath string) error {
	if status == "" {
		status = "training"
	}
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO trained_models
				(project_id, slug, task, version, training_dataset_signature,
				 eval_metrics, status, artifact_path)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			project, slug, task, version, trainingDatasetSignature,
			evalMetrics, status, artifactPath)
		if err != nil {
			return fmt.Errorf("insert on trained_models failed: %w", err)
		}
		return nil
	})
}

// trainedModelPromoteParams + HandleTrainedModelPromote.
//
// Promotion flips status to 'promoted'. By design, the action expects
// the row to currently be in 'ab_testing' (the A/B harness has
// accumulated enough comparisons that the promotion-gate projection
// has fired). Force-promote with `force=true` skips that check and is
// audited via the rationale envelope on mcp__toolkit-server__work.
//
// Identifier aliases: model may be named by slug or by the numeric id
// trained_model_list surfaces. id wins when both are supplied. Bug 1329
// parity for the trained_model surface.
type trainedModelPromoteParams struct {
	Slug  string `json:"slug"`
	ID    int64  `json:"id"`
	Force bool   `json:"force"`
}

// TrainedModelPromoteResult is the result type for trained_model_promote.
type TrainedModelPromoteResult struct {
	OK         bool   `json:"ok"`
	Slug       string `json:"slug,omitempty"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	Forced     bool   `json:"forced,omitempty"`
	Error      string `json:"error,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// HandleTrainedModelPromote implements work.trained_model_promote.
func HandleTrainedModelPromote(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TrainedModelPromoteResult, error) {
	var p trainedModelPromoteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TrainedModelPromoteResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" && p.ID > 0 {
		slug, err := lookupTrainedModelSlugByID(ctx, pool, p.ID)
		if err != nil {
			return TrainedModelPromoteResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return TrainedModelPromoteResult{Error: IdentifierRequiredError("trained_model_promote")}, nil
	}
	if project == "" {
		return TrainedModelPromoteResult{
			Error: "trained_model_promote requires a project scope",
			Hint:  "Pass project at the top-level envelope: work.action='trained_model_promote', project='mcp-servers', params={...}.",
		}, nil
	}

	currentStatus, err := readTrainedModelStatus(ctx, pool, project, p.Slug)
	if err != nil {
		if errors.Is(err, ErrTrainedModelNotFound) {
			return TrainedModelPromoteResult{
				Error: fmt.Sprintf("trained_model %q not found in project %q", p.Slug, project),
			}, nil
		}
		return TrainedModelPromoteResult{}, err
	}

	if currentStatus == "promoted" {
		return TrainedModelPromoteResult{
			OK:         true,
			Slug:       p.Slug,
			FromStatus: "promoted",
			ToStatus:   "promoted",
			Hint:       "no-op: model already promoted",
		}, nil
	}
	if !p.Force && currentStatus != "ab_testing" {
		return TrainedModelPromoteResult{
			Error: fmt.Sprintf("cannot promote from status %q without force=true", currentStatus),
			Hint:  "Promotion expects status='ab_testing' (A/B harness has accumulated comparisons). Pass {\"force\": true} to override — that path is audited via the work-surface rationale envelope.",
		}, nil
	}

	if err := writeTrainedModelStatus(ctx, pool, project, p.Slug, "promoted"); err != nil {
		return TrainedModelPromoteResult{}, err
	}

	return TrainedModelPromoteResult{
		OK:         true,
		Slug:       p.Slug,
		FromStatus: currentStatus,
		ToStatus:   "promoted",
		Forced:     p.Force && currentStatus != "ab_testing",
	}, nil
}

// trainedModelRetireParams + HandleTrainedModelRetire.
//
// Retirement flips status to 'retired' from any prior state. Reason is
// stored in the same trained_model row's eval_metrics JSON under the
// 'retirement_reason' key — keeps the lifecycle metadata co-located
// with the row.
type trainedModelRetireParams struct {
	Slug   string `json:"slug"`
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// TrainedModelRetireResult is the result type for trained_model_retire.
type TrainedModelRetireResult struct {
	OK         bool   `json:"ok"`
	Slug       string `json:"slug,omitempty"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	Error      string `json:"error,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// HandleTrainedModelRetire implements work.trained_model_retire.
func HandleTrainedModelRetire(ctx context.Context, pool *db.Pool, project string, params json.RawMessage) (TrainedModelRetireResult, error) {
	var p trainedModelRetireParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return TrainedModelRetireResult{}, fmt.Errorf("parse params: %w", err)
		}
	}
	if p.Slug == "" && p.ID > 0 {
		slug, err := lookupTrainedModelSlugByID(ctx, pool, p.ID)
		if err != nil {
			return TrainedModelRetireResult{Error: err.Error()}, nil
		}
		p.Slug = slug
	}
	if p.Slug == "" {
		return TrainedModelRetireResult{Error: IdentifierRequiredError("trained_model_retire")}, nil
	}
	if project == "" {
		return TrainedModelRetireResult{
			Error: "trained_model_retire requires a project scope",
			Hint:  "Pass project at the top-level envelope: work.action='trained_model_retire', project='mcp-servers', params={...}.",
		}, nil
	}

	currentStatus, err := readTrainedModelStatus(ctx, pool, project, p.Slug)
	if err != nil {
		if errors.Is(err, ErrTrainedModelNotFound) {
			return TrainedModelRetireResult{
				Error: fmt.Sprintf("trained_model %q not found in project %q", p.Slug, project),
			}, nil
		}
		return TrainedModelRetireResult{}, err
	}
	if currentStatus == "retired" {
		return TrainedModelRetireResult{
			OK:         true,
			Slug:       p.Slug,
			FromStatus: "retired",
			ToStatus:   "retired",
			Hint:       "no-op: model already retired",
		}, nil
	}

	if err := writeTrainedModelStatusWithReason(ctx, pool, project, p.Slug, "retired", p.Reason); err != nil {
		return TrainedModelRetireResult{}, err
	}

	return TrainedModelRetireResult{
		OK:         true,
		Slug:       p.Slug,
		FromStatus: currentStatus,
		ToStatus:   "retired",
	}, nil
}

// lookupTrainedModelSlugByID resolves a trained_models.id to its slug.
// Lets trained_model_promote / trained_model_retire accept {id} the way
// bug_resolve / bug_stamp_sha already do (bug 1329); the rest of each
// handler still operates on (project, slug) so the write SQL is
// unchanged regardless of how the caller named the model. The lookup
// is project-agnostic — id is unique across projects — so a non-matching
// (project, id) pair still surfaces via the downstream project-scoped
// readTrainedModelStatus call as ErrTrainedModelNotFound.
func lookupTrainedModelSlugByID(ctx context.Context, pool *db.Pool, id int64) (string, error) {
	var slug string
	err := pool.DB().QueryRowContext(ctx, `SELECT slug FROM trained_models WHERE id = ?`, id).Scan(&slug)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("trained_model id %d not found", id)
		}
		return "", err
	}
	return slug, nil
}

// readTrainedModelStatus is shared by promote / retire. Returns
// ErrTrainedModelNotFound when no row matches.
func readTrainedModelStatus(ctx context.Context, pool *db.Pool, project, slug string) (string, error) {
	var status string
	err := pool.DB().QueryRowContext(ctx,
		`SELECT status FROM trained_models WHERE project_id = ? AND slug = ?`,
		project, slug).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrTrainedModelNotFound
	}
	if err != nil {
		return "", err
	}
	return status, nil
}

// writeTrainedModelStatus updates status + bumps updated_at. Status enum
// is enforced by the CHECK constraint in migration 043 — invalid values
// surface as a constraint violation here, which is the right shape
// because the validator at the handler seam already rejected them.
func writeTrainedModelStatus(ctx context.Context, pool *db.Pool, project, slug, status string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE trained_models
			 SET status = ?, updated_at = datetime('now')
			 WHERE project_id = ? AND slug = ?`,
			status, project, slug)
		return err
	})
}

// writeTrainedModelStatusWithReason mirrors writeTrainedModelStatus but
// also merges a retirement reason into eval_metrics under the
// 'retirement_reason' key. The merge is a SQLite json_set so the
// existing metric values stay intact.
func writeTrainedModelStatusWithReason(ctx context.Context, pool *db.Pool, project, slug, status, reason string) error {
	return pool.WithWrite(ctx, func(tx *sql.Tx) error {
		if reason == "" {
			_, err := tx.ExecContext(ctx,
				`UPDATE trained_models
				 SET status = ?, updated_at = datetime('now')
				 WHERE project_id = ? AND slug = ?`,
				status, project, slug)
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE trained_models
			 SET status = ?,
			     eval_metrics = json_set(COALESCE(NULLIF(eval_metrics, ''), '{}'), '$.retirement_reason', ?),
			     updated_at = datetime('now')
			 WHERE project_id = ? AND slug = ?`,
			status, reason, project, slug)
		return err
	})
}

func scanTrainedModels(rows *sql.Rows) ([]TrainedModel, error) {
	// Non-nil zero-length slice so JSON marshals as `[]`, not `null`.
	// Callers rely on `[]` to distinguish "no matches" from "tool error".
	out := []TrainedModel{}
	for rows.Next() {
		var m TrainedModel
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Slug, &m.Task, &m.Version,
			&m.TrainingDatasetSignature, &m.EvalMetrics, &m.Status, &m.ArtifactPath,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanTrainedModelListItems(rows *sql.Rows) ([]TrainedModelListItem, error) {
	out := []TrainedModelListItem{}
	for rows.Next() {
		var m TrainedModelListItem
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.Slug, &m.Task, &m.Version,
			&m.Status, &m.ArtifactPath, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── Action-doc descriptors (parallel-run registry, action_doc.go) ───────

var trainedModelListDoc = ActionDoc{
	Purpose: "List trained_model rows. Filter by task or status. Status values: training | evaluating | ab_testing | promoted | retired. The registry under go/internal/ml/ uses this same filter set to resolve load-by-promoted lookups.",
	Params: []DocParam{
		{Name: "task", Required: false, Description: "Task identifier (e.g. 'source-router', 'cross-encoder-reranker'). Lowercase-kebab."},
		{Name: "status", Required: false, Description: "Lifecycle state filter: training | evaluating | ab_testing | promoted | retired. Invalid values reject with a typed error."},
		{Name: "verbose", Required: false, Description: "Return full rows (eval_metrics + training_dataset_signature included)."},
		{Name: "limit", Required: false, Description: "Default 50; clamped to >= 0."},
		{Name: "offset", Required: false, Description: "Pagination offset; rows to skip before applying limit."},
	},
	Example: `{"task":"source-router","status":"promoted"}`,
}

var trainedModelPromoteDoc = ActionDoc{
	Purpose: "Flip a trained_model row to status='promoted'. Expects current status='ab_testing' (the A/B harness has accumulated promotion-gate evidence). Pass {\"force\": true} to override from any state — that path is audited via the work-surface rationale envelope.",
	Params: []DocParam{
		// id-OR-slug one-of: HandleTrainedModelPromote resolves slug from id
		// (lookupTrainedModelSlugByID) and rejects only when both are empty
		// (IdentifierRequiredError). Neither arm is individually Required.
		{Name: "id", Required: false, Description: "trained_model row id (preferred — globally unique)."},
		{Name: "slug", Required: false, Description: "Slug of the trained_model row (convention: <task>-<version>)."},
		{Name: "force", Required: false, Description: "Skip the ab_testing-state precondition. Audited override."},
	},
	Example: `{"slug":"source-router-v1"}`,
}

var trainedModelRetireDoc = ActionDoc{
	Purpose: "Flip a trained_model row to status='retired' from any prior state. Optional reason merges into eval_metrics.retirement_reason for in-row lifecycle metadata.",
	Params: []DocParam{
		// id-OR-slug one-of: HandleTrainedModelRetire resolves slug from id
		// (lookupTrainedModelSlugByID) and rejects only when both are empty
		// (IdentifierRequiredError). Neither arm is individually Required.
		{Name: "id", Required: false, Description: "trained_model row id (preferred — globally unique)."},
		{Name: "slug", Required: false, Description: "Slug of the trained_model row."},
		{Name: "reason", Required: false, Description: "Free-form note explaining retirement; merged into eval_metrics.retirement_reason."},
	},
	Example: `{"slug":"source-router-v1","reason":"superseded by v2"}`,
}
