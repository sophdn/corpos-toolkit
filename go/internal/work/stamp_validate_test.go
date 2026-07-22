package work

import "testing"

// Regression for bug `task-stamp-sha-accepts-foreign-chain-commit`. The
// 882 scenario: SHA 598865b (subject "chain(arc-close-filing-review):
// T7 — …") was stamped onto a task in chain "train-skill-auto-loader-v1",
// silently phantom-closing the wrong task. commitChainMismatch is the
// guard's pure decision core — it must reject that cross-chain stamp
// while NOT false-rejecting legitimate same-chain stamps or
// unconventional commits that name no chain.
func TestCommitChainMismatch(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		chainSlug string
		taskSlug  string
		wantBlock bool
	}{
		{
			name:      "882 cross-chain stamp is rejected",
			body:      "chain(arc-close-filing-review): T7 — retrospective + closing audit event\n\nbody text",
			chainSlug: "train-skill-auto-loader-v1",
			taskSlug:  "t7-retrospective",
			wantBlock: true,
		},
		{
			name:      "same-chain stamp is allowed (subject names this chain)",
			body:      "chain(train-skill-auto-loader-v1): T7 — retrospective",
			chainSlug: "train-skill-auto-loader-v1",
			taskSlug:  "t7-retrospective",
			wantBlock: false,
		},
		{
			name:      "body references the task slug elsewhere — allowed",
			body:      "fix: land the t7-retrospective deliverable\n\ncloses t7-retrospective",
			chainSlug: "train-skill-auto-loader-v1",
			taskSlug:  "t7-retrospective",
			wantBlock: false,
		},
		{
			name:      "unconventional commit naming no chain — allowed (no false reject)",
			body:      "fix: refactor the widget loader\n\nno chain marker here",
			chainSlug: "train-skill-auto-loader-v1",
			taskSlug:  "t7-retrospective",
			wantBlock: false,
		},
		{
			name:      "empty body — allowed (can't judge)",
			body:      "",
			chainSlug: "train-skill-auto-loader-v1",
			taskSlug:  "t7-retrospective",
			wantBlock: false,
		},
		{
			name:      "chain prefix equal to this chain — allowed",
			body:      "chain(forge-vault-note-schema-rework): T6 — SKILL body",
			chainSlug: "forge-vault-note-schema-rework",
			taskSlug:  "t6-skill-body",
			wantBlock: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := commitChainMismatch(c.body, c.chainSlug, c.taskSlug)
			if c.wantBlock && got == "" {
				t.Errorf("expected a rejection, got allow (\"\")")
			}
			if !c.wantBlock && got != "" {
				t.Errorf("expected allow, got rejection: %q", got)
			}
		})
	}
}

func TestChainPrefixSlug(t *testing.T) {
	cases := map[string]string{
		"chain(arc-close-filing-review): T7 — x": "arc-close-filing-review",
		"chain(train-skill-auto-loader-v1): T1":  "train-skill-auto-loader-v1",
		"fix(work): bug `foo`":                   "",
		"feat: no chain prefix":                  "",
		"  chain(leading-space): T2":             "leading-space",
	}
	for subject, want := range cases {
		if got := chainPrefixSlug(subject); got != want {
			t.Errorf("chainPrefixSlug(%q) = %q, want %q", subject, got, want)
		}
	}
}
