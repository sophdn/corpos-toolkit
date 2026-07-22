package pointers

import "testing"

func TestNormalizeVaultSourceRef_StripsLegacyPrefix(t *testing.T) {
	got := NormalizeVaultSourceRef(".claude/vault/decisions/2026-05-11_foo.md")
	want := "decisions/2026-05-11_foo.md"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeVaultSourceRef_BareIsIdempotent(t *testing.T) {
	got := NormalizeVaultSourceRef("decisions/2026-05-11_foo.md")
	want := "decisions/2026-05-11_foo.md"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNormalizeVaultSourceRef_EmptyPassesThrough(t *testing.T) {
	if got := NormalizeVaultSourceRef(""); got != "" {
		t.Fatalf("empty input should pass through; got %q", got)
	}
}

func TestNormalizeVaultSourceRef_OnlyStripsAtStart(t *testing.T) {
	// Defensive: a substring match in the middle of the path is NOT a
	// legacy prefix — leave it alone. (Real vault paths never embed
	// '.claude/vault/' mid-string, but the function shouldn't
	// misbehave if a future contributor passes one.)
	got := NormalizeVaultSourceRef("decisions/.claude/vault/foo.md")
	want := "decisions/.claude/vault/foo.md"
	if got != want {
		t.Fatalf("expected %q (no strip), got %q", want, got)
	}
}
