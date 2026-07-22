package dispatch

import "path/filepath"

// ProjectPath pairs a project ID with its registered checkout path. The
// resolver matches a caller's Cwd against these to derive the effective
// project when no explicit project is supplied. Shared by the native stdio
// path (cmd/toolkit-server) and the HTTP path (internal/observehttp) so both
// transports resolve identically — historically only the stdio path was
// Cwd-aware, which is bug
// http-dispatch-ignores-per-session-default-project-header-and-cwd-uses-server-global.
type ProjectPath struct {
	ID   string
	Path string
}

// NewCwdProjectResolver returns a [ProjectResolver] that resolves, in order:
//
//  1. an explicit args.Project (caller override)
//  2. args.Cwd matched against paths (longest-prefix; caller pre-sorts paths
//     longest-first for determinism)
//  3. defaultProject
//
// Returns "" when none resolve. For a project-scoped WRITE that empty string
// surfaces the handler's "requires a project" error (fail loud, no silent
// misfiling); for a READ the dispatcher treats "" as cross-project. Callers
// that want a per-session default (e.g. the HTTP path's X-MCP-Default-Project
// header) pass it as defaultProject.
func NewCwdProjectResolver(paths []ProjectPath, defaultProject string) ProjectResolver {
	return func(args Args) string {
		if args.Project != "" {
			return args.Project
		}
		if args.Cwd != "" {
			for _, p := range paths {
				if args.Cwd == p.Path || HasPathPrefix(args.Cwd, p.Path) {
					return p.ID
				}
			}
		}
		return defaultProject
	}
}

// HasPathPrefix reports whether path starts with prefix and either equals
// prefix exactly or the next character is a path separator. Prevents
// /home/user/dev-other from matching /home/user/dev.
func HasPathPrefix(path, prefix string) bool {
	if len(path) < len(prefix) {
		return false
	}
	if path[:len(prefix)] != prefix {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	return path[len(prefix)] == filepath.Separator
}

// crossProjectReads is the allowlist of read-only actions that are
// cross-project by default — an unscoped call spans every project rather than
// inheriting the session/CWD/default project. Keyed "<surface>.<action>".
//
// This encodes the documented contract (actiondocs.WorkDescription: "Read
// actions are cross-project by default; pass 'project' to scope"). Without it,
// the resolver's default-project substitution silently collapses every
// unscoped read to one project — bug
// read-actions-inherit-session-default-project-breaking-cross-project-read-contract
// (bug_list returned 1 of 28 open bugs).
//
// This is an ALLOWLIST on purpose: it is fail-safe. An action omitted here
// keeps the resolver's project (the pre-fix behavior), so a mis-classification
// can only ever leave a forgotten READ mono-scoped — it can NEVER strip the
// default-project from a WRITE and misroute a create. That is why we do not
// key off the policy registry's requires_rationale flag: the policy TOML is
// deliberately an incomplete mutating registry (roadmap_update,
// roadmap_preview_set, task_unstart, trained_model_promote/retire are mutating
// but absent), so treating "absent from policy" as "read" would be fail-danger.
//
// Only the work surface scopes reads by project today; other surfaces' reads
// (knowledge vault/search, admin) are already cross-project or unscoped by
// construction. Add entries here as new project-scoping read actions land.
var crossProjectReads = map[string]bool{
	"work.bug_list":           true,
	"work.bug_read":           true,
	"work.task_list":          true,
	"work.task_search":        true,
	"work.task_read":          true,
	"work.task_blockers":      true,
	"work.suggestion_list":    true,
	"work.suggestion_read":    true,
	"work.chain_find":         true,
	"work.chain_state":        true,
	"work.chain_status":       true,
	"work.chain_dep_list":     true,
	"work.roadmap_list":       true,
	"work.roadmap_diff":       true,
	"work.roadmap_plan":       true,
	"work.trained_model_list": true,
	"work.recent_activity":    true,
	"work.where_we_left_off":  true,
}

// IsCrossProjectRead reports whether (surface, action) is a read that should
// span all projects when the caller supplies no explicit project. The
// dispatcher uses it to drop the resolver's default-project for such reads.
func IsCrossProjectRead(surface, action string) bool {
	return crossProjectReads[surface+"."+action]
}
