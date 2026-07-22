package spike

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"toolkit/internal/actiondocs"
)

// TestMeasure runs the keyword-match prototype against the InputSet
// and prints a table of (query, latency, top-3 hits, expected hit
// rank) so the spike's decision artifact can quote concrete numbers.
//
// Not a regression test in the usual sense — it doesn't fail unless
// the corpus can't load or the prototype panics. The output is the
// data the decision artifact summarizes. Run with: go test -tags
// sqlite_fts5 ./internal/actiondocs/spike/ -run TestMeasure -v
func TestMeasure(t *testing.T) {
	// The corpus is embedded in the binary (go/internal/actiondocs/corpus);
	// load it the same way production does rather than walking the tree.
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded corpus: %v", err)
	}
	if reg.Len() == 0 {
		t.Fatal("empty corpus; spike needs real chunks to measure")
	}

	t.Logf("corpus: %d chunks across %d surfaces (%v)", reg.Len(), len(reg.Surfaces()), reg.Surfaces())
	t.Logf("input set: %d queries", len(InputSet))

	var latencies []time.Duration
	var hits, misses int

	for i, q := range InputSet {
		start := time.Now()
		results := SearchKeyword(reg, q.Question, 3)
		elapsed := time.Since(start)
		latencies = append(latencies, elapsed)

		expRank := rankOf(results, q.ExpectedHit.Surface, q.ExpectedHit.Action)
		hitMsg := "MISS"
		if expRank >= 0 {
			hitMsg = fmt.Sprintf("HIT @ rank %d", expRank+1)
			hits++
		} else {
			misses++
		}

		t.Logf("\nQ%d (%.2fms, %s): %s", i+1, float64(elapsed.Microseconds())/1000.0, hitMsg, q.Question)
		t.Logf("    expected: %s.%s", q.ExpectedHit.Surface, q.ExpectedHit.Action)
		for j, h := range results {
			marker := "  "
			if h.Surface == q.ExpectedHit.Surface && h.Action == q.ExpectedHit.Action {
				marker = "→ "
			}
			t.Logf("    %s#%d %s.%s  (score=%.3f)", marker, j+1, h.Surface, h.Action, h.Score)
		}
	}

	summary := summarize(latencies)
	t.Logf("\n=== SUMMARY ===")
	t.Logf("hit-rate: %d/%d (%.0f%%)", hits, len(InputSet), 100.0*float64(hits)/float64(len(InputSet)))
	t.Logf("latency:  min=%.2fms  p50=%.2fms  max=%.2fms",
		ms(summary.min), ms(summary.p50), ms(summary.max))
}

func rankOf(hits []Hit, surface, action string) int {
	for i, h := range hits {
		if h.Surface == surface && h.Action == action {
			return i
		}
	}
	return -1
}

type latencyStats struct {
	min, p50, max time.Duration
}

func summarize(ds []time.Duration) latencyStats {
	if len(ds) == 0 {
		return latencyStats{}
	}
	sorted := append([]time.Duration(nil), ds...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	return latencyStats{
		min: sorted[0],
		p50: sorted[len(sorted)/2],
		max: sorted[len(sorted)-1],
	}
}

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// avoid unused-import on strings when iterating package layout
var _ = strings.Builder{}
