package benchmarks

import (
	"strings"
	"testing"
)

// SeedSnapshot covers two open chains + one closed chain, 15 tasks
// total. This pins the seed-data shape so a future scenario file that
// depends on a specific task slug (e.g. "ctms-read-tool" being closed)
// stays stable.
func TestSeedSnapshot_ShapeInvariants(t *testing.T) {
	if SeedSnapshot.ID != "seed-a" {
		t.Errorf("ID: want seed-a, got %q", SeedSnapshot.ID)
	}
	if len(SeedSnapshot.Chains) != 3 {
		t.Fatalf("Chains: want 3, got %d", len(SeedSnapshot.Chains))
	}

	want := map[string]struct {
		status string
		nTasks int
	}{
		"benchmark-l4-l5-extension": {"open", 5},
		"benchmark-page-redesign":   {"open", 5},
		"core-tools-mcp-surface":    {"closed", 5},
	}
	got := map[string]struct {
		status string
		nTasks int
	}{}
	for _, c := range SeedSnapshot.Chains {
		got[c.Slug] = struct {
			status string
			nTasks int
		}{c.Status, len(c.Tasks)}
	}
	for slug, w := range want {
		g, ok := got[slug]
		if !ok {
			t.Errorf("chain %q missing", slug)
			continue
		}
		if g.status != w.status {
			t.Errorf("chain %q status: want %q, got %q", slug, w.status, g.status)
		}
		if g.nTasks != w.nTasks {
			t.Errorf("chain %q nTasks: want %d, got %d", slug, w.nTasks, g.nTasks)
		}
	}

	totalTasks := 0
	for _, c := range SeedSnapshot.Chains {
		totalTasks += len(c.Tasks)
	}
	if totalTasks != 15 {
		t.Errorf("total tasks: want 15, got %d", totalTasks)
	}
}

// Render's output is the L4 prompt-injection format. Pin its exact shape
// since drift breaks downstream Qwen prompts that have been calibrated
// against this exact text block.
func TestProjectSnapshot_RenderProducesExpectedBlock(t *testing.T) {
	mini := ProjectSnapshot{
		ID: "test",
		Chains: []SnapshotChain{
			{
				Slug:   "alpha",
				Title:  "first chain",
				Status: "open",
				Tasks: []SnapshotTask{
					{Slug: "t1", Status: "active"},
					{Slug: "t2", Status: "pending"},
				},
			},
		},
	}
	got := mini.Render()
	want := "## Project context\n\n" +
		"Chain `alpha` (open) — first chain\n" +
		"  - `t1` (active)\n" +
		"  - `t2` (pending)\n" +
		"\n"
	if got != want {
		t.Errorf("Render output mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

// SeedSnapshot.Render is the actual fixture used by L4 scenarios. Pin
// its shape: heading, three chain paragraphs each with their task block,
// each separated by a blank line.
func TestSeedSnapshot_RenderShape(t *testing.T) {
	out := SeedSnapshot.Render()
	if !strings.HasPrefix(out, "## Project context\n\n") {
		t.Errorf("missing header")
	}
	for _, c := range SeedSnapshot.Chains {
		if !strings.Contains(out, "Chain `"+c.Slug+"` ("+c.Status+")") {
			t.Errorf("chain %q line missing", c.Slug)
		}
		for _, task := range c.Tasks {
			if !strings.Contains(out, "  - `"+task.Slug+"` ("+task.Status+")") {
				t.Errorf("task %q line missing under chain %q", task.Slug, c.Slug)
			}
		}
	}
}
