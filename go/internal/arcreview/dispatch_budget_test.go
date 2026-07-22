package arcreview

import (
	"errors"
	"strings"
	"testing"
)

// makeSnap builds a synthetic snapshot whose content runs to the
// requested approximate length so the budget-fit logic has something
// concrete to chew on. Tests use this rather than reading a fixture
// transcript because the budget check is pure-function over Snapshot.
func makeSnap(turnCount, contentCharsPerTurn int) Snapshot {
	msgs := make([]Message, turnCount)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = Message{
			Role:    role,
			Content: strings.Repeat("x", contentCharsPerTurn),
		}
	}
	return Snapshot{
		Messages:        msgs,
		EstimatedTokens: approxTokensForMessages(msgs),
	}
}

func TestEstimatePromptTokens_UsesCharsPerTokenEstimate(t *testing.T) {
	sys := strings.Repeat("s", 400)
	user := strings.Repeat("u", 400)
	got := estimatePromptTokens(sys, user)
	want := (400 + 400) / charsPerTokenEstimate
	if got != want {
		t.Fatalf("expected %d tokens, got %d", want, got)
	}
}

func TestMaxPromptTokens_FallsBackToDefault(t *testing.T) {
	t.Setenv("TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS", "")
	if got := maxPromptTokens(); got != defaultMaxPromptTokens {
		t.Fatalf("expected %d, got %d", defaultMaxPromptTokens, got)
	}
}

func TestMaxPromptTokens_HonorsEnvOverride(t *testing.T) {
	t.Setenv("TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS", "12345")
	if got := maxPromptTokens(); got != 12345 {
		t.Fatalf("expected 12345 from env, got %d", got)
	}
}

func TestMaxPromptTokens_RejectsNonsense(t *testing.T) {
	t.Setenv("TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS", "not-a-number")
	if got := maxPromptTokens(); got != defaultMaxPromptTokens {
		t.Fatalf("invalid env should fall back to default %d; got %d", defaultMaxPromptTokens, got)
	}
	t.Setenv("TOOLKIT_ARCREVIEW_MAX_PROMPT_TOKENS", "-100")
	if got := maxPromptTokens(); got != defaultMaxPromptTokens {
		t.Fatalf("negative env should fall back to default; got %d", got)
	}
}

func TestFitSnapshotToPromptBudget_FitsImmediatelyWhenUnderBudget(t *testing.T) {
	snap := makeSnap(5, 100) // ~500 chars of content, well under 6000 tokens
	out, trimmed, err := fitSnapshotToPromptBudget(snap, "short arc summary", []string{"event_bug_resolved"}, nil, defaultMaxPromptTokens)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if trimmed {
		t.Fatalf("snapshot was under budget; trim flag should be false")
	}
	if len(out.Messages) != 5 {
		t.Fatalf("expected 5 messages preserved; got %d", len(out.Messages))
	}
}

func TestFitSnapshotToPromptBudget_TrimsOldestUntilFits(t *testing.T) {
	// Build a snapshot whose contentCharsPerTurn pushes the total
	// prompt over a small budget. 20 turns of 1000 chars each = 20000
	// chars of content (~5000 tokens via the heuristic), plus the
	// system prompt's ~3500 chars (~875 tokens). Total ~5875 tokens.
	// Budget = 4000 → must trim.
	snap := makeSnap(20, 1000)
	out, trimmed, err := fitSnapshotToPromptBudget(snap, "arc summary", []string{"event_task_completed"}, nil, 4000)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !trimmed {
		t.Fatalf("expected trim=true on oversized snapshot")
	}
	if len(out.Messages) >= 20 {
		t.Fatalf("trim should have dropped at least one turn; still have %d", len(out.Messages))
	}
	if !out.Truncated {
		t.Fatalf("Truncated flag must be set after trimming")
	}
}

func TestFitSnapshotToPromptBudget_DropsOldestFirst(t *testing.T) {
	// Build a 10-turn snapshot with each turn's content marked with its
	// index so we can prove the OLDEST is dropped, not the newest.
	msgs := make([]Message, 10)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		// Each turn's content embeds the index AND padding to force
		// budget pressure.
		msgs[i] = Message{
			Role:    role,
			Content: "turn-" + string(rune('A'+i)) + " " + strings.Repeat("y", 600),
		}
	}
	snap := Snapshot{Messages: msgs}
	out, trimmed, err := fitSnapshotToPromptBudget(snap, "", nil, nil, 1500)
	if err != nil && !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("unexpected err: %v", err)
	}
	if !trimmed {
		t.Fatalf("expected trim=true")
	}
	// The kept messages should be from the TAIL of the original list;
	// turn-A (oldest) must have been dropped before turn-J (newest).
	for _, m := range out.Messages {
		if strings.HasPrefix(m.Content, "turn-A ") {
			t.Fatalf("oldest turn (turn-A) should have been trimmed first; still in output")
		}
	}
	if len(out.Messages) > 0 {
		last := out.Messages[len(out.Messages)-1]
		if !strings.HasPrefix(last.Content, "turn-J ") {
			t.Errorf("newest turn (turn-J) should be preserved; last is %q", last.Content[:10])
		}
	}
}

func TestFitSnapshotToPromptBudget_FloorAtMinSnapshotTurns(t *testing.T) {
	// Build a snapshot whose content is so dense that even
	// minSnapshotTurns can't fit. Each turn has 10000 chars (~2500
	// tokens via heuristic). With budget=1500, three turns alone =
	// ~7500 tokens — way over. Must return ErrPromptTooLarge.
	snap := makeSnap(15, 10000)
	out, trimmed, err := fitSnapshotToPromptBudget(snap, "", nil, nil, 1500)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("expected ErrPromptTooLarge; got %v", err)
	}
	if !trimmed {
		t.Fatalf("trim flag should be true even when we hit the floor")
	}
	if len(out.Messages) != minSnapshotTurns {
		t.Fatalf("expected snapshot trimmed to floor %d; got %d", minSnapshotTurns, len(out.Messages))
	}
}

func TestFitSnapshotToPromptBudget_RebuildsTokenEstimate(t *testing.T) {
	snap := makeSnap(10, 1000)
	originalTokens := snap.EstimatedTokens
	out, _, _ := fitSnapshotToPromptBudget(snap, "", nil, nil, 2000)
	if len(out.Messages) < 10 {
		// trimming happened; EstimatedTokens must have been recomputed
		// (smaller than original).
		if out.EstimatedTokens >= originalTokens {
			t.Errorf("EstimatedTokens should shrink after trimming; original=%d, after=%d", originalTokens, out.EstimatedTokens)
		}
	}
}
