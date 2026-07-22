package work

import (
	"os/exec"
	"regexp"
	"strings"
)

// Bug `task-stamp-sha-accepts-foreign-chain-commit`: task_stamp_sha did
// no relationship check between the stamped commit and the task's chain,
// so a mistyped task_id could stamp a commit that closes a DIFFERENT
// chain's task — a silent chain-state corruption (the wrong task shows
// closed; the right one stays pending). This file adds a best-effort
// guard.

// chainPrefixPattern extracts the chain slug from the conventional
// chain-closing commit subject `chain(<slug>): …`. The repo's chain-task
// commits follow this convention uniformly (git log confirms), so the
// captured slug is a high-confidence declaration of which chain the
// commit belongs to.
var chainPrefixPattern = regexp.MustCompile(`(?m)^\s*chain\(([^)\s]+)\)`)

// chainPrefixSlug returns the chain slug declared in a `chain(<slug>):`
// commit subject, or "" when the body carries no such prefix.
func chainPrefixSlug(body string) string {
	m := chainPrefixPattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// commitChainMismatch is the PURE decision core (no I/O) for the
// stamp-commit guard. Given a commit message body and the task's chain
// + task slugs, it returns a non-empty rejection reason ONLY when it can
// POSITIVELY determine the commit belongs to a different chain; it
// returns "" (allow) when the commit references this chain/task OR when
// there's no recognizable chain marker to judge by.
//
// Allow-by-default on ambiguity is deliberate: an unconventional but
// legitimate commit (one that doesn't name its chain) must not be
// blocked. The guard fires only on the high-confidence signal — the
// commit's `chain(<other>):` prefix names a chain that isn't this task's.
func commitChainMismatch(body, chainSlug, taskSlug string) string {
	if body == "" {
		return ""
	}
	// Expected stamp: the commit references this task's chain or the task
	// itself anywhere in the message.
	if chainSlug != "" && strings.Contains(body, chainSlug) {
		return ""
	}
	if taskSlug != "" && strings.Contains(body, taskSlug) {
		return ""
	}
	// High-confidence cross-chain signal: the conventional subject
	// declares a chain that differs from this task's chain.
	other := chainPrefixSlug(body)
	if other == "" || other == chainSlug {
		return ""
	}
	subject := firstLineTrimmed(body)
	return "commit declares it belongs to chain \"" + other +
		"\" (subject: \"" + subject + "\"), but you're stamping a task in chain \"" + chainSlug +
		"\" — this looks like a mistyped task id. Stamp the correct task; or if the commit genuinely closes this task, reference chain \"" +
		chainSlug + "\" in its message. Pass commit_sha=\"unversioned\" only for fixes that live outside any git repo."
}

// firstLineTrimmed returns the first line of s with surrounding
// whitespace trimmed.
func firstLineTrimmed(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// gitCommitMessage reads the full commit message body for sha in the
// repo at repoPath. Best-effort: returns ("", false) when repoPath is
// empty, git is unavailable, or the commit isn't in local history — the
// caller treats a false as "can't verify, allow the stamp" so an infra
// gap never blocks legitimate work.
func gitCommitMessage(repoPath, sha string) (string, bool) {
	if repoPath == "" || sha == "" {
		return "", false
	}
	cmd := exec.Command("git", "-C", repoPath, "show", "-s", "--format=%B", sha)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		return "", false
	}
	return body, true
}
