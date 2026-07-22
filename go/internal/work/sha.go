package work

// unversionedSentinel is accepted in place of a real commit SHA when the
// fixed artifact lives outside any git repo (e.g., user-level scripts
// under ~/.claude/, vault docs). Mirrors the Rust UNVERSIONED_SENTINEL.
const unversionedSentinel = "unversioned"

// isValidCommitSHA reports whether s is a 7-40 char ASCII-hex string.
// Mirrors Rust toolkit-server::dispatch::work::is_valid_commit_sha
// (bug 1195: bug_resolve / task_complete must reject arbitrary strings
// like "direct-file-edit"; the column is meant for git SHAs only).
func isValidCommitSHA(s string) bool {
	n := len(s)
	if n < 7 || n > 40 {
		return false
	}
	for i := 0; i < n; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// isValidCommitSHAOrSentinel accepts a real git SHA OR the
// "unversioned" sentinel.
func isValidCommitSHAOrSentinel(s string) bool {
	return s == unversionedSentinel || isValidCommitSHA(s)
}

// shaValidationError returns the canonical Rust error message for an
// invalid commit_sha. Variant carries the action-specific hint suffix
// (bug_resolve adds "Pass an empty string or omit ...").
func shaValidationError(sha string, withBugResolveHint bool) string {
	base := "commit_sha '" + sha + "' is not a valid git SHA — expected 7–40 hex chars (e.g. 'abc1234') or 'unversioned' for fixes living outside any git repo."
	if withBugResolveHint {
		base += " Pass an empty string or omit the field if the SHA is not yet known."
	}
	return base
}
