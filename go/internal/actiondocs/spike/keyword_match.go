// Package spike holds the throwaway prototype code for T5 of the
// action-docs-corpus chain. The Shape B prototype here is intentionally
// minimal: tokenize on whitespace + punctuation, score each chunk by
// query-term recall + per-term IDF, return top-3. No Qwen, no embeddings,
// no relevance feedback. The point is to measure whether even a naive
// matcher reduces the corpus-scanning cost enough to justify a real Q&A
// surface.
//
// NOT SHIPPED — this file lives under /spike/ so its presence in the
// import graph doesn't accidentally suggest the prototype is the
// recommended path. If a real Shape B lands, it goes in a sibling
// non-spike package after the spike's decision artifact recommends it.
package spike

import (
	"math"
	"sort"
	"strings"

	"toolkit/internal/actiondocs"
)

// Hit is one keyword-match result. Score is the IDF-weighted recall
// (sum of IDF over the query terms found in the chunk's bag-of-words).
// Higher = better.
type Hit struct {
	Surface string
	Action  string
	Score   float64
}

// SearchKeyword runs the prototype's keyword-match scoring across every
// chunk in the registry and returns the top-K results in descending
// Score order. Score ties are broken by (surface, action) lexicographic
// order for determinism.
//
// Tokenization is whitespace + punctuation; stopwords ("the", "a", "is",
// etc.) are dropped because they have IDF≈0 anyway but skipping the
// lookup saves cycles. Term matching is case-insensitive.
func SearchKeyword(reg *actiondocs.Registry, query string, topK int) []Hit {
	if reg == nil || reg.Len() == 0 {
		return nil
	}
	qTerms := tokenize(query)
	if len(qTerms) == 0 {
		return nil
	}

	// Gather every (surface, action, bag) so we can compute IDF over
	// the full corpus before per-chunk scoring.
	type chunkBag struct {
		surface string
		action  string
		bag     map[string]int
	}
	var chunks []chunkBag
	docFreq := make(map[string]int) // term → doc count
	for _, s := range reg.Surfaces() {
		// Sweep including _general so the cross-cutting chunks are scored.
		// Names(s) excludes _general; we add it explicitly when present.
		names := append([]string(nil), reg.Names(s)...)
		if _, ok := reg.Get(s, actiondocs.GeneralAction); ok {
			names = append(names, actiondocs.GeneralAction)
		}
		for _, a := range names {
			doc, ok := reg.Get(s, a)
			if !ok {
				continue
			}
			bag := chunkText(doc)
			chunks = append(chunks, chunkBag{s, a, bag})
			for t := range bag {
				docFreq[t]++
			}
		}
	}

	n := float64(len(chunks))
	scoreOne := func(bag map[string]int) float64 {
		var score float64
		for _, q := range qTerms {
			if tf := bag[q]; tf > 0 {
				// Smoothed IDF — log((N+1)/(df+1)) so even very common
				// terms get a tiny non-negative weight, and unseen
				// query terms (df=0) don't blow up.
				idf := math.Log((n + 1) / float64(docFreq[q]+1))
				// TF contribution is sqrt-damped so a chunk that
				// happens to repeat one query term twenty times
				// doesn't dominate over a chunk that hits three
				// different query terms once each.
				score += idf * math.Sqrt(float64(tf))
			}
		}
		return score
	}

	hits := make([]Hit, 0, len(chunks))
	for _, c := range chunks {
		s := scoreOne(c.bag)
		if s == 0 {
			continue
		}
		hits = append(hits, Hit{Surface: c.surface, Action: c.action, Score: s})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if hits[i].Surface != hits[j].Surface {
			return hits[i].Surface < hits[j].Surface
		}
		return hits[i].Action < hits[j].Action
	})
	if topK > 0 && len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

// chunkText returns the bag-of-words for one chunk. The full content
// (purpose + params + aliases + errors + notes) is folded into one
// term-frequency map so a single keyword match anywhere in the chunk
// counts. The prototype does NOT weight purpose higher than notes —
// that's a refinement question for a real implementation, not the
// spike.
func chunkText(doc *actiondocs.ActionDoc) map[string]int {
	bag := make(map[string]int)
	add := func(s string) {
		for _, t := range tokenize(s) {
			bag[t]++
		}
	}
	add(doc.Surface)
	add(doc.Action)
	add(doc.Purpose)
	for _, p := range doc.Params {
		add(p.Name)
		add(p.Type)
		add(p.Description)
	}
	for _, a := range doc.ParamAliases {
		add(a.From)
		add(a.To)
		add(a.Notes)
	}
	for _, a := range doc.ValueAliases {
		add(a.Param)
		add(a.From)
		add(a.To)
		add(a.Notes)
	}
	for _, e := range doc.Errors {
		add(e.Condition)
		add(e.Message)
	}
	for _, ex := range doc.Examples {
		add(ex.Description)
		add(ex.Call)
	}
	add(doc.Notes)
	return bag
}

// tokenize splits on whitespace + ASCII punctuation, lowercases, drops
// stopwords + tokens shorter than two chars. Deliberately naive; the
// spike is measuring whether even THIS works, not whether a polished
// tokenizer would.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var out []string
	var cur strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			cur.WriteRune(r)
		default:
			if cur.Len() > 0 {
				t := cur.String()
				cur.Reset()
				if len(t) < 2 {
					continue
				}
				if _, drop := stopwords[t]; drop {
					continue
				}
				out = append(out, t)
			}
		}
	}
	if cur.Len() > 0 {
		t := cur.String()
		if len(t) >= 2 {
			if _, drop := stopwords[t]; !drop {
				out = append(out, t)
			}
		}
	}
	return out
}

// stopwords is a tiny hand-rolled English stopword set — enough to
// suppress the noisiest function words in the input set's natural-
// language framing without dropping any term that carries domain
// signal. Names of toolkit fields (action, surface, sha, kind) are
// intentionally NOT stopworded.
var stopwords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "are": {}, "but": {},
	"not": {}, "you": {}, "can": {}, "with": {}, "this": {},
	"that": {}, "from": {}, "have": {}, "has": {}, "does": {},
	"do": {}, "what": {}, "when": {}, "how": {}, "why": {},
	"which": {}, "where": {}, "who": {}, "whom": {}, "is": {},
	"was": {}, "be": {}, "to": {}, "of": {}, "in": {},
	"on": {}, "at": {}, "by": {}, "an": {}, "or": {},
	"as": {}, "if": {}, "it": {}, "its": {}, "than": {},
	"into": {}, "out": {}, "via": {}, "off": {}, "ok": {},
}
