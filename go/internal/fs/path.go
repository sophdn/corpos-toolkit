package fs

import (
	"os"
	"path/filepath"
	"strings"
)

// expandUserPath expands a leading ~ in path to the current user's home
// directory: a bare "~" becomes $HOME and "~/x" becomes $HOME/x. Any other path
// — absolute, relative, or a "~user" form we deliberately do not resolve — is
// returned unchanged.
//
// This is the single tilde-resolution point for the fs surface. Without it a
// leading ~ is not special to the OS, so filepath.Abs / os.Stat resolve "~/x"
// against the daemon's working directory and silently create "<cwd>/~/x" — a
// literal "~" directory — while reporting success (bug
// fs-surface-does-not-expand-leading-tilde). Every fs handler routes its
// caller-supplied path through this helper before touching the filesystem so the
// path lands where the caller meant.
func expandUserPath(path string) string {
	if path == "~" {
		if home := homeDir(); home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home := homeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// homeDir returns the current user's home directory, or "" when it cannot be
// resolved (in which case the caller leaves the path untouched rather than
// guessing — a "~/x" that cannot be expanded is better surfaced as a missing
// path than silently written to the wrong place).
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}
