package main

import (
	"strings"
	"testing"
)

// ── qvsScore() — mode → shape + per-criterion subscore ─────────────────
//
// 9 Rust tests mirrored 1:1. Mode handling is the load-bearing decision
// the dispatch table makes; honesty case in particular has the tricky
// invariant that accuracy mirrors honesty (refusal IS the correct
// answer).

func TestQVSScore_RoutingPassesWhenGoldMatchesResponse(t *testing.T) {
	got := qvsScore("routing", "decisions/", "decisions/")
	if got.err != nil {
		t.Fatalf("err: %v", got.err)
	}
	if got.shape != "Classify" {
		t.Errorf("shape: want Classify, got %s", got.shape)
	}
	if got.accuracy != 1.0 {
		t.Errorf("accuracy: want 1.0, got %v", got.accuracy)
	}
	if got.ranking != nil {
		t.Errorf("ranking: want nil, got %v", *got.ranking)
	}
	if got.honesty != nil {
		t.Errorf("honesty: want nil, got %v", *got.honesty)
	}
}

func TestQVSScore_RoutingFailsWhenResponseDiffers(t *testing.T) {
	got := qvsScore("routing", "reference/", "learnings/general/")
	if got.accuracy != 0.0 {
		t.Errorf("accuracy: want 0.0, got %v", got.accuracy)
	}
}

func TestQVSScore_RoutingIsCaseInsensitive(t *testing.T) {
	got := qvsScore("routing", "Decisions/", "DECISIONS/")
	if got.accuracy != 1.0 {
		t.Errorf("accuracy: want 1.0, got %v", got.accuracy)
	}
}

func TestQVSScore_RetrievalReturnsRetrieveWithRankingQuality(t *testing.T) {
	got := qvsScore("retrieval", "decisions/foo.md", "decisions/foo.md")
	if got.shape != "Retrieve" {
		t.Errorf("shape: want Retrieve, got %s", got.shape)
	}
	if got.accuracy != 1.0 {
		t.Errorf("accuracy: want 1.0, got %v", got.accuracy)
	}
	if got.ranking == nil || *got.ranking != 1.0 {
		t.Errorf("ranking: want 1.0, got %v", got.ranking)
	}
	if got.honesty != nil {
		t.Errorf("honesty: want nil, got %v", *got.honesty)
	}
}

func TestQVSScore_RetrievalPartialCreditWhenGoldInResponseButNotRankOne(t *testing.T) {
	got := qvsScore("retrieval", "decisions/foo.md", "decisions/other.md\ndecisions/foo.md")
	if got.accuracy != 1.0 {
		t.Errorf("accuracy: want 1.0 (gold present anywhere), got %v", got.accuracy)
	}
	if got.ranking == nil || *got.ranking != 0.0 {
		t.Errorf("ranking: want 0.0 (gold on line 2), got %v", got.ranking)
	}
}

func TestQVSScore_RetrievalFailsBothWhenGoldAbsent(t *testing.T) {
	got := qvsScore("retrieval", "decisions/foo.md", "decisions/other.md")
	if got.accuracy != 0.0 {
		t.Errorf("accuracy: want 0.0, got %v", got.accuracy)
	}
	if got.ranking == nil || *got.ranking != 0.0 {
		t.Errorf("ranking: want 0.0, got %v", got.ranking)
	}
}

func TestQVSScore_HonestyPassesWhenResponseSaysNoMatch(t *testing.T) {
	got := qvsScore("honesty", "no match", "no match")
	if got.shape != "Retrieve" {
		t.Errorf("shape: want Retrieve, got %s", got.shape)
	}
	if got.honesty == nil || *got.honesty != 1.0 {
		t.Errorf("honesty: want 1.0, got %v", got.honesty)
	}
	if got.accuracy != 1.0 {
		t.Errorf("accuracy: want 1.0 (honesty case mirrors honesty), got %v", got.accuracy)
	}
	if got.ranking == nil || *got.ranking != 1.0 {
		t.Errorf("ranking: want 1.0 (honest no-match = no spurious ranking), got %v", got.ranking)
	}
}

func TestQVSScore_HonestyFailsWhenResponsePicksAPath(t *testing.T) {
	got := qvsScore("honesty", "no match", "decisions/2026-05-05_toolkit-server-canonical-forge.md")
	if got.honesty == nil || *got.honesty != 0.0 {
		t.Errorf("honesty: want 0.0, got %v", got.honesty)
	}
	if got.accuracy != 0.0 {
		t.Errorf("accuracy: want 0.0, got %v", got.accuracy)
	}
	if got.ranking == nil || *got.ranking != 0.0 {
		t.Errorf("ranking: want 0.0, got %v", got.ranking)
	}
}

func TestQVSScore_UnknownModeErrors(t *testing.T) {
	got := qvsScore("invalid_mode", "x", "y")
	if got.err == nil || !strings.Contains(got.err.Error(), "unknown mode") {
		t.Errorf("expected unknown-mode error, got %v", got.err)
	}
}

// ── qvsParseISO8601Z — fixed YYYY-MM-DDTHH:MM:SSZ shape ──────────────

func TestQVSParseISO8601Z_UnixEpoch(t *testing.T) {
	got, err := qvsParseISO8601Z("1970-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}

func TestQVSParseISO8601Z_KnownTimestamp(t *testing.T) {
	got, err := qvsParseISO8601Z("2026-05-08T06:39:47Z")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 1778222387 {
		t.Errorf("want 1778222387, got %d", got)
	}
}

func TestQVSParseISO8601Z_RejectsWrongLength(t *testing.T) {
	if _, err := qvsParseISO8601Z("2026-05-08"); err == nil {
		t.Errorf("expected error for short string")
	}
	if _, err := qvsParseISO8601Z("2026-05-08T06:39:47.000Z"); err == nil {
		t.Errorf("expected error for sub-second precision (binary uses strict format)")
	}
}

func TestQVSParseISO8601Z_RejectsMissingTOrZ(t *testing.T) {
	if _, err := qvsParseISO8601Z("2026-05-08 06:39:47Z"); err == nil {
		t.Errorf("expected error for space instead of T")
	}
	if _, err := qvsParseISO8601Z("2026-05-08T06:39:47 "); err == nil {
		t.Errorf("expected error for missing Z")
	}
}

// ── qvsStripGGUFSuffix — model name normalization ─────────────────────

func TestQVSStripGGUFSuffix_NormalizesQwenQuantPath(t *testing.T) {
	got := qvsStripGGUFSuffix("Qwen2.5-32B-Instruct-Q4_K_M.gguf")
	if got != "qwen2.5-32b" {
		t.Errorf("want qwen2.5-32b, got %s", got)
	}
}

func TestQVSStripGGUFSuffix_PassesThroughCanonicalNames(t *testing.T) {
	if got := qvsStripGGUFSuffix("qwen2.5-32b"); got != "qwen2.5-32b" {
		t.Errorf("want qwen2.5-32b, got %s", got)
	}
	if got := qvsStripGGUFSuffix("claude-haiku-4-5-20251001"); got != "claude-haiku-4-5-20251001" {
		t.Errorf("want claude-haiku-4-5-20251001, got %s", got)
	}
}
