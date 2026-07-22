package qwenretrieve

import (
	"path/filepath"
	"strings"
)

// CorpusShape selects the retrieve system prompt and the user-prompt header.
// Vault is the personal knowledge vault (author notes under ~/.claude/vault/).
// Kiwix is the offline encyclopedic corpus addressed by <zim_id>/<slug>.
type CorpusShape int

const (
	// CorpusShapeVault is the personal knowledge vault shape (default).
	CorpusShapeVault CorpusShape = iota
	// CorpusShapeKiwix is the offline encyclopedic-corpus shape.
	CorpusShapeKiwix
)

// RetrieveCandidate is one entry the model ranks. Title/Summary are pointers so
// the caller can omit absent fields without conflating "absent" with empty.
// BodyExcerpt is populated only for pass-2 calls (Context.WithBody == true).
type RetrieveCandidate struct {
	Path        string
	Title       *string
	Tags        []string
	Summary     *string
	BodyExcerpt *string
}

// RetrieveTaskInput carries the query + top_k cap.
type RetrieveTaskInput struct {
	Query string
	TopK  int
}

// RetrieveContext carries the candidate list, pass selector, and corpus shape.
// WithBody=false is pass-1 (full corpus list with title/tags/summary). WithBody=true
// is pass-2 (a small candidate list enriched with body excerpts).
type RetrieveContext struct {
	Candidates  []RetrieveCandidate
	WithBody    bool
	CorpusShape CorpusShape
}

const retrievePass1VaultSystem = `You rank notes from a personal knowledge vault against a task description. ` +
	`Each note is presented as a path, an optional title, optional tags, and an ` +
	`optional one-line summary. Reply with relative paths chosen from the ` +
	`supplied list, one per line, in order of relevance (most relevant first), ` +
	`and nothing else.

Match by TOPIC, not by lexical overlap. Use the title, tags, and summary ` +
	`to bridge vocabulary gaps with the path. A note about "retrieval" or ` +
	`"RAG" or "embeddings" is on-topic for queries about "ranking" or ` +
	`"search" or "finding things in a corpus" — these are the same domain ` +
	`under different vocabulary. A note about "benchmarks" or "metrics" is ` +
	`on-topic for queries about "observability" or "tracking performance". ` +
	`A note about "forge" or "dispatch" is on-topic for queries about ` +
	`"how the meta-tool works" or "adding new actions". Synonyms, hypernyms, ` +
	`and adjacent-concept matches all count. The vault is small enough that ` +
	`a second-pass query is expensive — over-include rather than under-include.

If fewer than the requested count match, reply with as many as do. ` +
	`Reply with the single line 'no match' ONLY when truly none of the notes ` +
	`are on the topic at all (e.g. a query about kubernetes against a vault ` +
	`with no container/orchestration content).`

const retrievePass2VaultSystem = `You re-rank a small candidate list of vault notes against a task description. ` +
	`For each candidate you receive a path, title, tags, and a body excerpt ` +
	`(first ~700 chars). Reply with the requested number of relative paths ` +
	`chosen from the candidate list, one per line, in order of relevance ` +
	`(most relevant first), and nothing else.

The candidate list has been pre-filtered to be plausibly on-topic. Your ` +
	`job is to RANK them by relevance based on the body excerpt — not just ` +
	`the title. The agent calling this is doing context-gathering: a note ` +
	`that touches the task's DOMAIN is useful to surface even when it doesn't ` +
	`directly answer the task's specific sub-question.

Two-rule preference order:
1. RELEVANCE FIRST — RANK, DON'T DROP. Read the body excerpt and rank ` +
	`candidates by how much they touch the task's domain. Include any ` +
	`candidate whose body engages with the task's broader topic. Drop a ` +
	`candidate only when its body discusses a clearly DIFFERENT problem ` +
	`(e.g. a note tagged 'rag' whose body is actually about cron jobs).
2. SPECIFICITY SECOND. A dated learning note (` + "`learnings/.../2026-MM-DD_topic.md`" + `) ` +
	`that directly addresses the task usually outranks a broad survey or ` +
	`design-doc reference (` + "`reference/topic.md`" + `, ` + "`decisions/topic.md`" + `) on the ` +
	`same domain — the dated note was written precisely because the survey ` +
	`wasn't enough. But the broad survey still RANKS, just lower.

Worked example: for the task 'prior art on small-corpus retrieval', ` +
	"`learnings/general/2026-05-08_small-corpus-retrieval-skip-embeddings.md`" + ` ` +
	`outranks ` + "`reference/rag-architecture.md`" + ` — but BOTH appear in the reply ` +
	`since both touch the small-corpus retrieval domain.

If fewer than the requested count touch the domain at all, reply with ` +
	`as many as do. Reply with the single line 'no match' ONLY when every ` +
	`candidate's body discusses a clearly different problem from the task.`

const retrievePass1KiwixSystem = `You rank encyclopedic articles from offline reference corpora against a ` +
	`task description. Each article is presented as a path (zim_id/slug), an ` +
	`optional title, and a search-engine snippet excerpting the article body. ` +
	`Reply with article paths chosen from the supplied list, one per line, in ` +
	`order of relevance (most relevant first), and nothing else.

Match by TOPIC, not by lexical overlap. Use the title and snippet to ` +
	`bridge vocabulary gaps with the path. An article titled "Aliasing ` +
	`(computing)" is on-topic for queries about "raw pointers" or "borrow ` +
	`checker" — these are the same domain under different vocabulary. An ` +
	`article titled "Future (programming)" is on-topic for queries about ` +
	`"async" or "tokio runtime" or "promise". An article titled ` +
	`"Database normalization" is on-topic for queries about "schema design" ` +
	`or "data modelling". Synonyms, hypernyms, and adjacent-concept matches ` +
	`all count. The hit list is bounded (typically 10–20 entries returned by ` +
	`the kiwix search) — over-include rather than under-include when an ` +
	`article's snippet plausibly touches the topic.

If fewer than the requested count match, reply with as many as do. ` +
	`Reply with the single line 'no match' ONLY when truly none of the ` +
	`articles are on the topic at all (e.g. an internal-codebase term like ` +
	`"toolkit-server forge action" against a Wikipedia or programming-docs ` +
	`corpus that doesn't index it).`

const retrievePass2KiwixSystem = `You re-rank a small candidate list of encyclopedic articles against a ` +
	`task description. For each candidate you receive a path (zim_id/slug), ` +
	`an optional title, and a body excerpt (first ~700 chars of the article). ` +
	`Reply with the requested number of article paths chosen from the ` +
	`candidate list, one per line, in order of relevance (most relevant ` +
	`first), and nothing else.

The candidate list has been pre-filtered to be plausibly on-topic. Your ` +
	`job is to RANK them by relevance based on the body excerpt — not just ` +
	`the title. The agent calling this is doing context-gathering: an article ` +
	`that touches the task's DOMAIN is useful to surface even when it doesn't ` +
	`directly answer the task's specific sub-question.

Two-rule preference order:
1. RELEVANCE FIRST — RANK, DON'T DROP. Read the body excerpt and rank ` +
	`candidates by how much they touch the task's domain. Include any ` +
	`candidate whose body engages with the task's broader topic. Drop a ` +
	`candidate only when its body discusses a clearly DIFFERENT problem ` +
	`(e.g. a candidate titled ` + "`Aliasing (sociology)`" + ` whose body is about ` +
	`political coalitions, against a query about pointer aliasing).
2. SPECIFICITY SECOND. An article on a narrow, task-shaped topic ` +
	`(e.g. ` + "`Tokio_(software)`" + ` for an async-runtime query) usually outranks a ` +
	`broad survey on the same domain (e.g. ` + "`Rust_(programming_language)`" + `) — ` +
	`the narrow article addresses the specific question while the survey ` +
	`covers it as one bullet among many. But the broad survey still RANKS, ` +
	`just lower.

If fewer than the requested count touch the domain at all, reply with ` +
	`as many as do. Reply with the single line 'no match' ONLY when every ` +
	`candidate's body discusses a clearly different problem from the task.`

// ComposeRetrieve builds the (system, user) prompt pair for one retrieve call.
// Ported from inference_clients::dispatcher::compose::compose_retrieve.
func ComposeRetrieve(task RetrieveTaskInput, ctx RetrieveContext) (string, string) {
	var out strings.Builder

	switch {
	case ctx.WithBody && ctx.CorpusShape == CorpusShapeVault:
		out.WriteString("Candidate notes (with body excerpts):\n\n")
	case ctx.WithBody && ctx.CorpusShape == CorpusShapeKiwix:
		out.WriteString("Candidate articles (with body excerpts):\n\n")
	case !ctx.WithBody && ctx.CorpusShape == CorpusShapeVault:
		out.WriteString("Vault notes (relative paths under the vault root):\n\n")
	default: // !WithBody, Kiwix
		out.WriteString("Articles (paths shaped <zim_id>/<slug>):\n\n")
	}

	for _, cand := range ctx.Candidates {
		out.WriteString("- ")
		out.WriteString(cand.Path)
		out.WriteByte('\n')

		// Title: skip when it duplicates the filename stem or full path.
		stem := pathStem(cand.Path)
		if cand.Title != nil && *cand.Title != "" && *cand.Title != stem && *cand.Title != cand.Path {
			out.WriteString("  title: ")
			out.WriteString(*cand.Title)
			out.WriteByte('\n')
		}

		if len(cand.Tags) > 0 {
			out.WriteString("  tags: ")
			out.WriteString(strings.Join(cand.Tags, ", "))
			out.WriteByte('\n')
		}

		if !ctx.WithBody {
			if cand.Summary != nil && *cand.Summary != "" {
				out.WriteString("  summary: ")
				out.WriteString(*cand.Summary)
				out.WriteByte('\n')
			}
		} else if cand.BodyExcerpt != nil && *cand.BodyExcerpt != "" {
			out.WriteString("  body: |\n")
			for _, line := range strings.Split(*cand.BodyExcerpt, "\n") {
				out.WriteString("    ")
				out.WriteString(line)
				out.WriteByte('\n')
			}
		}
	}

	itemNoun := "notes"
	singleNoun := "note"
	if ctx.CorpusShape == CorpusShapeKiwix {
		itemNoun = "articles"
		singleNoun = "article"
	}

	escaped := strings.ReplaceAll(task.Query, `"`, `\"`)
	if ctx.WithBody {
		out.WriteString("\nTask: \"")
		out.WriteString(escaped)
		out.WriteString("\"\n\nPick the top ")
		out.WriteString(itoa(task.TopK))
		out.WriteString(" most relevant candidates. ")
		out.WriteString("Reply with up to ")
		out.WriteString(itoa(task.TopK))
		out.WriteString(" relative paths from the candidate list above, ")
		out.WriteString("one per line, no explanation, no commentary, no header. Apply the ")
		out.WriteString("RELEVANCE-FIRST then SPECIFICITY-SECOND rule from the system prompt.")
	} else {
		out.WriteString("\nTask: \"")
		out.WriteString(escaped)
		out.WriteString("\"\n\nPick the top ")
		out.WriteString(itoa(task.TopK))
		out.WriteString(" most relevant ")
		out.WriteString(itemNoun)
		out.WriteString(". Reply with up to ")
		out.WriteString(itoa(task.TopK))
		out.WriteString(" relative paths from the list above, one per line, ")
		out.WriteString("no explanation. If a ")
		out.WriteString(singleNoun)
		out.WriteString(" is on the topic of the task — even when ")
		out.WriteString("phrased differently from the task — include it. Reply 'no match' ")
		out.WriteString("only when none of the ")
		out.WriteString(itemNoun)
		out.WriteString(" are on the topic at all.")
	}

	var system string
	switch {
	case !ctx.WithBody && ctx.CorpusShape == CorpusShapeVault:
		system = retrievePass1VaultSystem
	case ctx.WithBody && ctx.CorpusShape == CorpusShapeVault:
		system = retrievePass2VaultSystem
	case !ctx.WithBody && ctx.CorpusShape == CorpusShapeKiwix:
		system = retrievePass1KiwixSystem
	default:
		system = retrievePass2KiwixSystem
	}

	return system, out.String()
}

// pathStem returns the filename stem (last segment with extension removed).
func pathStem(p string) string {
	base := filepath.Base(p)
	for i := len(base) - 1; i >= 0; i-- {
		if base[i] == '.' {
			return base[:i]
		}
	}
	return base
}

// itoa is a small wrapper to avoid pulling in strconv for one call per format.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
