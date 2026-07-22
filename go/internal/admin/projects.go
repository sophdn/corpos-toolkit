package admin

import (
	"context"
	"encoding/json"
	"errors"
)

type projectRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

// ProjectRegisterResult is the response shape for project_register.
type ProjectRegisterResult struct {
	OK        bool   `json:"ok"`
	ProjectID string `json:"project_id"`
}

// projectRegisterParams is the typed project_register request body — the
// json.Unmarshal target AND the action-doc TYPE source: adminActionRegistry
// reflects it (reflect.TypeOf(projectRegisterParams{})) so each param's type
// derives from the field kind rather than being re-authored (chain
// finalize-action-docs-epic T4, bug 943; docs/ACTION_DOC_CONTRACT.md). Hoisted
// from the prior inline anonymous struct — same fields, json tags, and unmarshal,
// so the binding is byte-for-byte unchanged. id + name required-ness is enforced
// by the handler guard below (not the unmarshal), so the descriptor authors
// Required=true for them.
type projectRegisterParams struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

func (d Deps) projectRegister(ctx context.Context, params json.RawMessage) (ProjectRegisterResult, error) {
	var p projectRegisterParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return ProjectRegisterResult{}, err
		}
	}
	if p.ID == "" || p.Name == "" {
		return ProjectRegisterResult{}, errors.New("params.id and params.name are required")
	}
	// Mirrors shared_db::register_project — idempotent upsert.
	_, err := d.Pool.DB().ExecContext(ctx,
		`INSERT INTO projects (id, name, path) VALUES (?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET name = excluded.name, path = excluded.path`,
		p.ID, p.Name, p.Path,
	)
	if err != nil {
		return ProjectRegisterResult{}, err
	}
	return ProjectRegisterResult{OK: true, ProjectID: p.ID}, nil
}

func (d Deps) projectList(ctx context.Context) ([]projectRow, error) {
	rows, err := d.Pool.DB().QueryContext(ctx,
		`SELECT id, name, path, created_at FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []projectRow{}
	for rows.Next() {
		var p projectRow
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
