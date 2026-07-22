// Command action-docs-suggest is the Shape C hook's per-call worker:
// it reads a query string from CLI args (or stdin), runs the spike's
// keyword-match against the action-docs corpus, and prints the top-3
// hits as JSON. The Shape C hook script invokes this from
// UserPromptSubmit so the agent gets a contextual suggestion injected
// alongside their prompt.
//
// NOT SHIPPED — the binary lives under the spike's directory tree so
// its presence in the build graph doesn't suggest the hook is the
// recommended path. If a real Shape C ships, the binary moves to a
// non-spike location after the spike's decision artifact recommends it.
//
// Usage:
//
//	action-docs-suggest <corpus_dir> <query>
//	action-docs-suggest <corpus_dir>     # reads query from stdin
//
// The corpus_dir path is required so the binary doesn't bake a
// system-specific default. The hook script resolves it (typically
// $HOME/dev/mcp-servers/go/internal/actiondocs/corpus) before invoking.
package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"

	"toolkit/internal/actiondocs"
	"toolkit/internal/actiondocs/spike"
)

type suggestion struct {
	Surface string  `json:"surface"`
	Action  string  `json:"action"`
	Score   float64 `json:"score"`
}

type output struct {
	Query       string       `json:"query"`
	Suggestions []suggestion `json:"suggestions"`
	Error       string       `json:"error,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		emit(output{Error: "usage: action-docs-suggest <corpus_dir> [query]"})
		os.Exit(2)
	}
	corpusDir := os.Args[1]
	var query string
	if len(os.Args) >= 3 {
		query = os.Args[2]
	} else {
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			emit(output{Error: "stdin read: " + err.Error()})
			os.Exit(2)
		}
		query = string(b)
	}
	if query == "" {
		emit(output{Error: "empty query"})
		return
	}

	reg, err := actiondocs.Load(corpusDir)
	if err != nil {
		emit(output{Query: query, Error: "load corpus: " + err.Error()})
		return
	}
	hits := spike.SearchKeyword(reg, query, 3)
	out := output{Query: query, Suggestions: make([]suggestion, 0, len(hits))}
	for _, h := range hits {
		out.Suggestions = append(out.Suggestions, suggestion{
			Surface: h.Surface,
			Action:  h.Action,
			Score:   h.Score,
		})
	}
	emit(out)
}

func emit(o output) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(o)
}
