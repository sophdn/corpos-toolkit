package work

import "testing"

func TestIsValidCommitSHA(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc1234", true},
		{"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", true}, // 40 chars
		{"ABC1234", true}, // uppercase hex
		{"", false},
		{"abc123", false}, // 6 chars — too short
		{"abc12345abc12345abc12345abc12345abc12345a", false}, // 41 chars
		{"not-a-sha", false},
		{"direct-file-edit", false}, // bug 1195 — must still be rejected
		{"abcdefg", false},          // 'g' is not hex
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := isValidCommitSHA(c.in); got != c.want {
				t.Errorf("isValidCommitSHA(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestIsValidCommitSHAOrSentinel(t *testing.T) {
	if !isValidCommitSHAOrSentinel("unversioned") {
		t.Error("unversioned sentinel should be accepted")
	}
	if isValidCommitSHAOrSentinel("Unversioned") {
		t.Error("sentinel must be case-sensitive")
	}
	if isValidCommitSHAOrSentinel("unversion") {
		t.Error("partial sentinel should be rejected")
	}
	if !isValidCommitSHAOrSentinel("abc1234") {
		t.Error("valid SHA should pass")
	}
}
