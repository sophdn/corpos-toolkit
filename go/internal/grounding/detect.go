package grounding

import (
	"encoding/json"
	"regexp"
	"strings"

	"toolkit/internal/telemetry"
)

// minCitationChars is the lower bound for the "≥40 contiguous chars from
// the result body" branch of the cited tier (TT1 §5.3). The threshold
// lives here, not on the schema, because TT1 §5.2 documents it as
// per-installation tunable in ~/.config/toolkit-server/click-weights.toml.
// The conservative default ships with the binary; future runtime override
// reads the same value before invoking the detectors.
const minCitationChars = 40

// Interaction is the in-memory equivalent of telemetry.InteractionArgs
// — the click-kind detectors return these, and the main orchestrator
// translates them into telemetry.EmitInteraction calls. Carrying the
// extra Source-event linkage (Action, SourceRefIndex) lets test code
// assert against shape without touching the DB.
type Interaction struct {
	SourceRef          string
	ClickKind          telemetry.ClickKind
	Position           int
	CitationKind       *telemetry.CitationKind
	CitationQuoteChars *int
	DwellMS            *int
}

// detectInteractions runs all four click_kind detectors against the
// records following the search call and returns one Interaction per
// (source_ref, kind) signal that fires. Multiple kinds may fire for the
// same source_ref — TT1 §5.1 — each is its own row.
//
// Scope: detectors stop at the next promptId boundary. The TT1 design
// scopes followed/cited/mentioned to the same prompt arc (§5 says
// "within the same span_id" for followed, but that's the parent search
// span — practically the next action against the candidate happens in a
// later tools/call in the same prompt arc); resolved-from is also
// prompt-scoped (§5.4) since the trajectory key is prompt_id.
func detectInteractions(ev ProcessedEvent, entries []jsonlEntry) []Interaction {
	if len(ev.SourceRefs) == 0 {
		return nil
	}

	// Cut entries at the next promptId boundary so detectors don't bleed
	// signal across user-input arcs. An empty ev.PromptID means the
	// transcript didn't surface a prompt_id for this search; the same
	// scope rule applies (stop at the first record whose threaded
	// promptId differs from "").
	scope := scopeWithinPrompt(entries[ev.RecordIdx+1:], ev.PromptID)
	if len(scope) == 0 {
		return nil
	}

	var out []Interaction

	// Position map: SourceRef → 1-indexed rank in the result list.
	pos := make(map[string]int, len(ev.SourceRefs))
	for i, ref := range ev.SourceRefs {
		if _, exists := pos[ref]; !exists {
			pos[ref] = i + 1
		}
	}

	followed := detectFollowed(ev.SourceRefs, pos, scope)
	out = append(out, followed...)

	cited := detectCited(ev.SourceRefs, pos, ev.ResultText, scope)
	out = append(out, cited...)

	mentioned := detectMentioned(ev.SourceRefs, pos, scope)
	out = append(out, mentioned...)

	return out
}

// scopeWithinPrompt returns the prefix of entries whose threaded
// promptId equals scope. The scope ends at the first record whose
// promptId differs — that's the next user-input arc. Records with an
// empty promptId that follow the scoped arc count as "in scope" only
// when scope is also empty (transcripts where promptId never landed).
func scopeWithinPrompt(entries []jsonlEntry, scope string) []jsonlEntry {
	for i, e := range entries {
		if e.PromptID != scope {
			return entries[:i]
		}
	}
	return entries
}

// followMode selects how a followIntent matches a candidate source_ref.
type followMode int

const (
	followNone  followMode = iota
	followPath             // filesystem-style: suffix-match a path (Read, vault_read)
	followKiwix            // "<zim>::<slug>" exact match (kiwix_fetch)
	followSlug             // scheme + terminal-slug match (work/library/skill verbs)
)

// followIntent is the normalized "the agent acted on this candidate"
// signal extracted from a single tool_use. Exactly one match mode is
// active. It generalises the old vault/kiwix-only `followed` detection:
// bug follow-detector-only-credits-vault-kiwix-read found that the
// detector recognised only native Read, vault_read, and kiwix_fetch, so
// every other surface an agent acts on (a chain via work.chain_state, a
// bug via bug_read, a skill via the Skill tool, a library entry via
// library_get) recorded NO interaction — leaving those candidates
// permanently stuck at negative/hard_negative in
// proj_training_data_for_reranker. followIntent + matchesRef restore the
// missing 1.0 `followed` signal for those schemes.
type followIntent struct {
	mode   followMode
	path   string // followPath: the file/vault path the agent read
	kiwix  string // followKiwix: "<zim>::<slug>"
	scheme string // followSlug: source_ref scheme (chain/task/bug/suggestion/library/skill)
	proj   string // followSlug: project, when known ("" = unknown, skip the project check)
	ident  string // followSlug: terminal slug / name to match
}

// followIntentFor classifies a tool_use as a follow signal, or returns
// followNone. The recognised "I'm following up on this specific
// candidate" verbs, by surface:
//
//	native Read                      → followPath (input.file_path)
//	Skill tool                       → followSlug skill   (input.skill)
//	knowledge vault_read             → followPath (params.path)
//	knowledge kiwix_fetch            → followKiwix (params.zim_id+slug)
//	knowledge library_get            → followSlug library (params.slug)
//	work chain_state / chain_status  → followSlug chain   (params.slug)
//	work task_read                   → followSlug task    (params.task_slug|slug)
//	work bug_read                    → followSlug bug     (params.slug)
//	work suggestion_read             → followSlug suggestion (params.slug)
//
// Bare-survey readers (Grep, Glob) and id-only reads (bug_read by numeric
// id, which carries no slug to match) deliberately return followNone —
// they are not candidate-specific follow signals.
func followIntentFor(toolName, action string, input map[string]json.RawMessage) followIntent {
	if toolName == "Read" {
		var p string
		if raw, ok := input["file_path"]; ok {
			_ = json.Unmarshal(raw, &p)
		}
		if p != "" {
			return followIntent{mode: followPath, path: p}
		}
		return followIntent{}
	}
	if toolName == "Skill" {
		var name string
		if raw, ok := input["skill"]; ok {
			_ = json.Unmarshal(raw, &name)
		}
		if name != "" {
			return followIntent{mode: followSlug, scheme: "skill", ident: name}
		}
		return followIntent{}
	}

	// MCP meta-tools dispatch by `action`; `project` rides at input level
	// (sibling of action/params), matching resolve.go's terminal-event read.
	var proj string
	if raw, ok := input["project"]; ok {
		_ = json.Unmarshal(raw, &proj)
	}
	var params struct {
		Path     string `json:"path"`
		ZimID    string `json:"zim_id"`
		Slug     string `json:"slug"`
		TaskSlug string `json:"task_slug"`
	}
	if raw, ok := input["params"]; ok {
		_ = json.Unmarshal(raw, &params)
	}

	switch {
	case strings.Contains(toolName, "knowledge"):
		switch action {
		case "vault_read":
			if params.Path != "" {
				return followIntent{mode: followPath, path: params.Path}
			}
		case "kiwix_fetch":
			if params.ZimID != "" && params.Slug != "" {
				return followIntent{mode: followKiwix, kiwix: params.ZimID + "::" + params.Slug}
			}
		case "library_get":
			if params.Slug != "" {
				return followIntent{mode: followSlug, scheme: "library", proj: proj, ident: params.Slug}
			}
		}
	case strings.Contains(toolName, "work"):
		switch action {
		case "chain_state", "chain_status":
			if params.Slug != "" {
				return followIntent{mode: followSlug, scheme: "chain", proj: proj, ident: params.Slug}
			}
		case "task_read":
			ident := params.TaskSlug
			if ident == "" {
				ident = params.Slug
			}
			if ident != "" {
				return followIntent{mode: followSlug, scheme: "task", proj: proj, ident: ident}
			}
		case "bug_read":
			if params.Slug != "" {
				return followIntent{mode: followSlug, scheme: "bug", proj: proj, ident: params.Slug}
			}
		case "suggestion_read":
			if params.Slug != "" {
				return followIntent{mode: followSlug, scheme: "suggestion", proj: proj, ident: params.Slug}
			}
		}
	}
	return followIntent{}
}

// matchesRef reports whether this follow intent targets the given
// candidate source_ref.
func (fi followIntent) matchesRef(ref string) bool {
	switch fi.mode {
	case followPath:
		// Strip any "<scheme>:" so both the bare-path form vault_search
		// emits ("learnings/general/foo.md") and the scheme-prefixed form
		// knowledge_search emits ("vault:.claude/vault/.../foo.md") resolve
		// against the same read.
		return pathMatches(fi.path, stripScheme(ref))
	case followKiwix:
		return ref == fi.kiwix
	case followSlug:
		scheme, proj, terminal := refIdentity(ref)
		if scheme != fi.scheme {
			return false
		}
		// Project check only when both sides know it — suggestion refs
		// carry no project component, and id-form reads carry no project.
		if fi.proj != "" && proj != "" && !strings.EqualFold(proj, fi.proj) {
			return false
		}
		return terminal != "" && strings.EqualFold(terminal, fi.ident)
	}
	return false
}

// detectFollowed scans the scope for tool_use calls that follow up on one
// of the search results and emits a `followed` interaction per matched
// (source_ref) pair. Each pair fires at most once per scope per TT1 §5.1
// — the UNIQUE (span_id, source_ref, click_kind) constraint upserts
// duplicates anyway, but skip them here for efficiency.
func detectFollowed(refs []string, pos map[string]int, scope []jsonlEntry) []Interaction {
	fired := make(map[string]bool, len(refs))
	var out []Interaction
	for _, e := range scope {
		if e.Type != "assistant" || e.Message == nil {
			continue
		}
		var content []json.RawMessage
		if err := json.Unmarshal(e.Message.Content, &content); err != nil {
			continue
		}
		for _, item := range content {
			var head struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(item, &head); err != nil {
				continue
			}
			if head.Type != "tool_use" {
				continue
			}
			var w struct {
				Input map[string]json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(item, &w); err != nil {
				continue
			}
			var action string
			if a, ok := w.Input["action"]; ok {
				_ = json.Unmarshal(a, &action)
			}
			intent := followIntentFor(head.Name, action, w.Input)
			if intent.mode == followNone {
				continue
			}
			for _, ref := range refs {
				if fired[ref] {
					continue
				}
				if !intent.matchesRef(ref) {
					continue
				}
				fired[ref] = true
				out = append(out, Interaction{
					SourceRef: ref,
					ClickKind: telemetry.ClickFollowed,
					Position:  pos[ref],
				})
			}
		}
	}
	return out
}

// splitScheme separates a leading "<scheme>:" prefix from a source_ref.
// Only a lowercase [a-z_]+ scheme immediately followed by ':' (and not the
// "::" separator) counts; bare paths ("learnings/general/foo.md") and
// "<zim>::<slug>" kiwix refs return ok=false with the ref unchanged.
func splitScheme(ref string) (scheme, body string, ok bool) {
	i := strings.IndexByte(ref, ':')
	if i <= 0 || i+1 >= len(ref) || ref[i+1] == ':' {
		return "", ref, false
	}
	for _, r := range ref[:i] {
		if (r < 'a' || r > 'z') && r != '_' {
			return "", ref, false
		}
	}
	return ref[:i], ref[i+1:], true
}

// stripScheme returns a source_ref's body with any "<scheme>:" prefix
// removed, for path-suffix matching.
func stripScheme(ref string) string {
	_, body, _ := splitScheme(ref)
	return body
}

// refIdentity decomposes a scheme-prefixed source_ref into (scheme,
// project, terminal-slug) for slug-style follow matching. Formats handled
// (from the live knowledge_pointers corpus):
//
//	chain:<proj>::<slug>          → ("chain","<proj>","<slug>")
//	task:<proj>::<chain>::<task>  → ("task","<proj>","<task>")
//	bug:<proj>::<slug>            → ("bug","<proj>","<slug>")
//	suggestion:<slug>             → ("suggestion","","<slug>")
//	library:<proj>::<dewey>       → ("library","<proj>","<dewey>")
//	skill:skills/<name>.toml      → ("skill","","<name>")
//
// A "::"-bearing body splits to (first=project, last=terminal); a "/"-
// bearing body resolves to its basename stem; otherwise the body is the
// terminal. A ref with no scheme prefix yields scheme "".
func refIdentity(ref string) (scheme, proj, terminal string) {
	scheme, body, ok := splitScheme(ref)
	if !ok {
		return "", "", body
	}
	switch {
	case strings.Contains(body, "::"):
		parts := strings.Split(body, "::")
		proj = parts[0]
		terminal = parts[len(parts)-1]
	case strings.Contains(body, "/"):
		terminal = basenameStem(body)
	default:
		terminal = body
	}
	return scheme, proj, terminal
}

// basenameStem returns the final path component of p with any file
// extension stripped: "skills/reference-resolution.toml" → "reference-resolution".
func basenameStem(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndexByte(p, '.'); i > 0 {
		p = p[:i]
	}
	return p
}

// proseMentions reports whether lowerText (already lowercased) references ref
// in slug form — the shared prose-match predicate for the `mentioned` and
// `resolved-from` detectors. Two token derivations are tried:
//
//   - normalizeSlug(ref): the vault/kiwix tail form (a date-prefixed note stem,
//     or the kiwix "<zim>::<slug>" passthrough). Matched as a plain substring —
//     these forms are highly distinctive, so the historical behavior (and the
//     TestNormalizeSlug-pinned shapes) are preserved exactly.
//   - refIdentity(ref).terminal: for a scheme-prefixed ref (chain:/bug:/task:/
//     skill:/suggestion:/library:/…), normalizeSlug leaves the scheme + "::"
//     literal intact, which agents never type in prose — they write the bare
//     terminal slug ("action-docs-corpus", not "chain:mcp-servers::action-docs-
//     corpus"). That terminal is matched on word boundaries with a min-length /
//     has-letter guard so a short or numeric terminal can't over-fire inside a
//     larger word (bug 958; a `mentioned` is only a 0.4 signal, so we stay
//     conservative). The vault/kiwix TestNormalizeSlug behavior is untouched
//     because normalizeSlug itself is unchanged.
func proseMentions(lowerText, ref string) bool {
	if slug := normalizeSlug(ref); slug != "" &&
		strings.Contains(lowerText, strings.ToLower(slug)) {
		return true
	}
	if scheme, _, terminal := refIdentity(ref); scheme != "" && isDistinctiveTerminal(terminal) {
		return containsWord(lowerText, strings.ToLower(terminal))
	}
	return false
}

// isDistinctiveTerminal guards the scheme-terminal prose-match path: a terminal
// slug is used as a mention token only when it is ≥4 runes and carries a letter,
// so bare-numeric or trivially-short terminals don't seed false mentions.
func isDistinctiveTerminal(terminal string) bool {
	if len([]rune(terminal)) < 4 {
		return false
	}
	for _, r := range terminal {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// containsWord reports whether token occurs in s delimited by non-alphanumeric
// boundaries, so a slug like "init" matches the standalone word but not the
// "init" inside "reinitialize". Both args must be lowercased by the caller.
func containsWord(s, token string) bool {
	if token == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(s[from:], token)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(token)
		if !isAlnumByte(s, start-1) && !isAlnumByte(s, end) {
			return true
		}
		from = start + 1
	}
}

// isAlnumByte reports whether s[i] is an ASCII lowercase letter or digit
// (s is lowercased by the caller). Out-of-range indices are non-alnum, so the
// string edges count as word boundaries.
func isAlnumByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// pathMatches accepts an exact match OR a candidate path whose tail
// equals the source_ref. Agents often Read with a relative path while
// the search returned a vault-rooted path (`learnings/general/foo.md`
// vs `/home/user/.claude/vault/learnings/general/foo.md`). The tail
// suffix match covers both forms.
func pathMatches(candidate, sourceRef string) bool {
	if candidate == sourceRef {
		return true
	}
	if strings.HasSuffix(candidate, "/"+sourceRef) {
		return true
	}
	if strings.HasSuffix(sourceRef, "/"+candidate) {
		return true
	}
	return false
}

// detectCited scans the scope for assistant-text citations of each
// result. Three patterns count per TT1 §5.3 / TT1.5 §4:
//
//   - markdown-link: [...](source_ref) — exact ref in the URL slot
//   - file-line: source_ref followed by ":<line>" (e.g. main.go:42)
//   - quoted-block: ≥minCitationChars contiguous chars from the result
//     body (when the result body is available; vault_search responses
//     don't currently carry an excerpt, so this branch is mostly a
//     kiwix-snippet path until vault-search-result shapes grow body
//     text).
func detectCited(refs []string, pos map[string]int, resultText string, scope []jsonlEntry) []Interaction {
	// One firing per source_ref. If multiple patterns match for the same
	// ref, we keep the first one — the consumer can re-derive via the
	// per-row citation_kind for finer aggregation. The 0.8 default weight
	// is per-tier, not per-pattern.
	fired := make(map[string]bool, len(refs))
	bodies := extractResultBodies(resultText)

	var out []Interaction
	for _, e := range scope {
		if e.Type != "assistant" || e.Message == nil {
			continue
		}
		var content []json.RawMessage
		if err := json.Unmarshal(e.Message.Content, &content); err != nil {
			continue
		}
		for _, item := range content {
			text, ok := decodeAssistantText(item)
			if !ok {
				continue
			}
			for i, ref := range refs {
				if fired[ref] {
					continue
				}
				body := ""
				if i < len(bodies) {
					body = bodies[i]
				}
				kind, chars, hit := matchCitation(ref, body, text)
				if !hit {
					continue
				}
				fired[ref] = true
				ck := kind
				out = append(out, Interaction{
					SourceRef:          ref,
					ClickKind:          telemetry.ClickCited,
					Position:           pos[ref],
					CitationKind:       &ck,
					CitationQuoteChars: chars,
				})
			}
		}
	}
	return out
}

// matchCitation returns the citation_kind sub-classifier (TT1 §5.3) for
// the first pattern that matches in text. Body is the optional result
// body to test the ≥minCitationChars contiguous-quote branch against.
// Returns (kind, quote-length, true) on hit; (_, _, false) otherwise.
func matchCitation(sourceRef, body, text string) (telemetry.CitationKind, *int, bool) {
	// Pattern 1: markdown link [anchor](source_ref).
	if mdLinkRe.MatchString(text) && strings.Contains(text, "]("+sourceRef+")") {
		return telemetry.CitationMarkdownLink, nil, true
	}
	// Pattern 2: file:line reference. Look for "<source_ref>:<digits>"
	// where digits is at least one character. Avoid generic substring
	// matches that aren't followed by a line number.
	if idx := strings.Index(text, sourceRef+":"); idx >= 0 {
		after := text[idx+len(sourceRef)+1:]
		if len(after) > 0 && after[0] >= '0' && after[0] <= '9' {
			return telemetry.CitationFileLine, nil, true
		}
	}
	// Pattern 3: contiguous ≥minCitationChars substring of body in text.
	if body != "" && len(body) >= minCitationChars {
		if best := longestCommonSubstringLen(body, text); best >= minCitationChars {
			b := best
			return telemetry.CitationQuotedBlock, &b, true
		}
	}
	return "", nil, false
}

// longestCommonSubstringLen returns the length of the longest contiguous
// substring shared by a and b. O(len(a)*len(b)) time and space — the
// inputs are bounded by a single tool_result body and a single assistant
// turn, both KB-scale at most. For large bodies we'd switch to a
// suffix automaton; not needed at the homelab scale TT1 §10.3 frames.
func longestCommonSubstringLen(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	// Cap inputs to keep the matrix bounded. Real tool_result bodies are
	// already ≤8 KiB per the scanner cap; assistant turns can be larger.
	const cap = 4096
	if len(a) > cap {
		a = a[:cap]
	}
	if len(b) > cap {
		b = b[:cap]
	}
	rowsA := []rune(a)
	rowsB := []rune(b)
	prev := make([]int, len(rowsB)+1)
	curr := make([]int, len(rowsB)+1)
	best := 0
	for i := 1; i <= len(rowsA); i++ {
		for j := 1; j <= len(rowsB); j++ {
			if rowsA[i-1] == rowsB[j-1] {
				curr[j] = prev[j-1] + 1
				if curr[j] > best {
					best = curr[j]
				}
			} else {
				curr[j] = 0
			}
		}
		prev, curr = curr, prev
		for j := range curr {
			curr[j] = 0
		}
	}
	return best
}

// extractResultBodies pulls the per-result text body from the parsed
// tool_result content. Vault_search responses don't currently include
// body text; kiwix_search includes `snippet` per SearchHit; knowledge_search
// includes `excerpt` per KnowledgeHit. The detector treats missing fields
// as no-body — the markdown-link and file-line citation paths still fire.
func extractResultBodies(resultText string) []string {
	if resultText == "" {
		return nil
	}
	// Vault search
	var v struct {
		Results []struct {
			Excerpt string `json:"excerpt"`
			Content string `json:"content"`
			Body    string `json:"body"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(resultText), &v); err == nil && len(v.Results) > 0 {
		out := make([]string, len(v.Results))
		for i, r := range v.Results {
			switch {
			case r.Excerpt != "":
				out[i] = r.Excerpt
			case r.Content != "":
				out[i] = r.Content
			case r.Body != "":
				out[i] = r.Body
			}
		}
		return out
	}
	// Kiwix search
	var k struct {
		Hits []struct {
			Snippet string `json:"snippet"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(resultText), &k); err == nil && len(k.Hits) > 0 {
		out := make([]string, len(k.Hits))
		for i, h := range k.Hits {
			out[i] = h.Snippet
		}
		return out
	}
	// Knowledge search (flat array)
	var n []struct {
		Excerpt string `json:"excerpt"`
		Snippet string `json:"snippet"`
		Body    string `json:"body"`
	}
	if err := json.Unmarshal([]byte(resultText), &n); err == nil && len(n) > 0 {
		out := make([]string, len(n))
		for i, r := range n {
			switch {
			case r.Excerpt != "":
				out[i] = r.Excerpt
			case r.Snippet != "":
				out[i] = r.Snippet
			case r.Body != "":
				out[i] = r.Body
			}
		}
		return out
	}
	return nil
}

var mdLinkRe = regexp.MustCompile(`\]\([^)]+\)`)

// detectMentioned scans the scope's assistant-text turns for slug-form
// references to each result. Per TT1.5 §7.1, 80%+ of mentions in real
// transcripts use the slug form (the date-prefixed tail without the
// path prefix or .md suffix), so the detector normalizes both sides.
// The slug-form match takes precedence over the strict full-path match.
func detectMentioned(refs []string, pos map[string]int, scope []jsonlEntry) []Interaction {
	fired := make(map[string]bool, len(refs))
	var out []Interaction
	for _, e := range scope {
		if e.Type != "assistant" || e.Message == nil {
			continue
		}
		var content []json.RawMessage
		if err := json.Unmarshal(e.Message.Content, &content); err != nil {
			continue
		}
		for _, item := range content {
			text, ok := decodeAssistantText(item)
			if !ok {
				continue
			}
			lower := strings.ToLower(text)
			for _, ref := range refs {
				if fired[ref] {
					continue
				}
				if !proseMentions(lower, ref) {
					continue
				}
				fired[ref] = true
				out = append(out, Interaction{
					SourceRef: ref,
					ClickKind: telemetry.ClickMentioned,
					Position:  pos[ref],
				})
			}
		}
	}
	return out
}

// decodeAssistantText returns the textual body of a content block when
// the block is {type:"text", text:"..."}. Other block types (tool_use,
// thinking, etc.) return ("", false).
func decodeAssistantText(item json.RawMessage) (string, bool) {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(item, &b); err != nil {
		return "", false
	}
	if b.Type != "text" {
		return "", false
	}
	if strings.TrimSpace(b.Text) == "" {
		return "", false
	}
	return b.Text, true
}
