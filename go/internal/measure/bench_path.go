package measure

import (
	"context"
	"os"
	"path/filepath"

	"toolkit/internal/db"
)

// lookupProjectPath returns the registering project's filesystem root
// (projects.path). Empty when unset/unknown — callers fall back to the
// process cwd. Mirrors the work-package helper of the same name; bench
// path resolution needs it so repo-relative binary_path / baseline_json_path
// registrations resolve against the project root rather than the
// project-agnostic stdio MCP cwd (~/dev).
func lookupProjectPath(ctx context.Context, pool *db.Pool, projectID string) string {
	if pool == nil {
		return ""
	}
	var path string
	if err := pool.DB().QueryRowContext(ctx,
		`SELECT path FROM projects WHERE id = ?`, projectID,
	).Scan(&path); err != nil {
		return ""
	}
	return path
}

// resolveBenchPath turns a harness path into an absolute one. Absolute
// (and empty) paths pass through unchanged. A relative path joins the
// project root when known, else the process cwd as a best-effort fallback
// so an unregistered project keeps the pre-fix behavior.
func resolveBenchPath(p, projectRoot string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if projectRoot != "" {
		return filepath.Join(projectRoot, p)
	}
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, p)
	}
	return p
}
