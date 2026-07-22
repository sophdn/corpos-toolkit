package construct

import (
	"encoding/json"
	"fmt"
	"strings"

	"toolkit/internal/forge/fieldvalue"
)

// ChainTaskEntry is one entry in a forge(chain).tasks list — either the
// legacy pipe-delimited shape (Mode == ChainTaskModePipe) carrying just
// slug + scope, or the T7 full-object shape (Mode == ChainTaskModeFull)
// carrying the full task field set + a per-task rationale.
//
// Distinguishing the two at parse time is per-entry (a single tasks
// list MAY mix both shapes); the validator routes each entry through
// the appropriate creation path during chainInsertTaskSkeletonsFromField.
//
// T7 of work-batching-and-forge-templates.
type ChainTaskEntry struct {
	Mode ChainTaskMode

	// Slug is required for both modes — it's the task identifier in
	// the chain's namespace.
	Slug string

	// Pipe-shape fields (Mode == ChainTaskModePipe).
	Scope  string
	Status string

	// Full-shape fields (Mode == ChainTaskModeFull). Per-task rationale
	// is mandatory on this shape; missing rejects the whole forge call
	// before any DB write.
	ProblemStatement   string
	AcceptanceCriteria []string
	ContextRequired    string
	Constraints        string
	Rationale          string
}

// ChainTaskMode names the entry-shape variant.
type ChainTaskMode string

const (
	ChainTaskModePipe ChainTaskMode = "pipe_delimited"
	ChainTaskModeFull ChainTaskMode = "full_objects"
)

// ParseChainTasks decodes a forge(chain).tasks raw JSON value into a
// typed slice of entries. Accepts:
//
//   - JSON null / empty / absent → returns nil + nil error
//   - JSON string "slug|scope|status" → 1-element pipe slice (single-task
//     chain coerces from the single-string shape)
//   - JSON array of strings → all pipe-delimited (legacy)
//   - JSON array of objects → all full-object (T7 new shape)
//   - JSON array of mixed strings + objects → mixed (per-entry detection)
//
// Errors when a full-object entry is missing required fields (slug or
// rationale) — the caller surfaces these as a forge rejection BEFORE
// any DB write so the chain create is atomic with the rejection.
func ParseChainTasks(raw json.RawMessage) ([]ChainTaskEntry, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Trim leading whitespace to peek the first byte.
	trimmed := raw
	for len(trimmed) > 0 {
		switch trimmed[0] {
		case ' ', '\t', '\n', '\r':
			trimmed = trimmed[1:]
			continue
		}
		break
	}
	if len(trimmed) == 0 {
		return nil, nil
	}
	// Single string → pipe shape, 1 entry.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, fmt.Errorf("tasks single-string parse: %w", err)
		}
		if s == "" {
			return nil, nil
		}
		entry, err := parsePipeEntry(s)
		if err != nil {
			return nil, err
		}
		return []ChainTaskEntry{entry}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("tasks must be a string or JSON array; got first byte %q", string(trimmed[0]))
	}
	var rawEntries []json.RawMessage
	if err := json.Unmarshal(trimmed, &rawEntries); err != nil {
		return nil, fmt.Errorf("tasks array parse: %w", err)
	}
	out := make([]ChainTaskEntry, 0, len(rawEntries))
	for i, re := range rawEntries {
		entry, err := parseChainTaskEntry(re)
		if err != nil {
			return nil, fmt.Errorf("tasks[%d]: %w", i, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// parseChainTaskEntry handles one entry's per-entry shape detection.
func parseChainTaskEntry(raw json.RawMessage) (ChainTaskEntry, error) {
	trimmed := raw
	for len(trimmed) > 0 {
		switch trimmed[0] {
		case ' ', '\t', '\n', '\r':
			trimmed = trimmed[1:]
			continue
		}
		break
	}
	if len(trimmed) == 0 {
		return ChainTaskEntry{}, fmt.Errorf("empty entry")
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return ChainTaskEntry{}, fmt.Errorf("pipe-shape parse: %w", err)
		}
		return parsePipeEntry(s)
	}
	if trimmed[0] == '{' {
		return parseFullObjectEntry(trimmed)
	}
	return ChainTaskEntry{}, fmt.Errorf("entry must be a string or object; got first byte %q", string(trimmed[0]))
}

func parsePipeEntry(s string) (ChainTaskEntry, error) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 2 {
		return ChainTaskEntry{}, fmt.Errorf("pipe entry missing scope: %q", s)
	}
	slug := strings.TrimSpace(parts[0])
	if slug == "" {
		return ChainTaskEntry{}, fmt.Errorf("pipe entry missing slug: %q", s)
	}
	scope := strings.TrimSpace(parts[1])
	status := ""
	if len(parts) >= 3 {
		status = strings.TrimSpace(parts[2])
	}
	return ChainTaskEntry{
		Mode:   ChainTaskModePipe,
		Slug:   slug,
		Scope:  scope,
		Status: status,
	}, nil
}

func parseFullObjectEntry(raw json.RawMessage) (ChainTaskEntry, error) {
	// Use json.RawMessage for acceptance_criteria AND context_required so we can
	// accept BOTH the single-string shape AND the list shape, matching the
	// underlying task schema's optional_string_or_list field type and standalone
	// forge(task) (bug 1086: context_required was typed plain `string` here, so the
	// list shape forge(task) accepts was rejected with "cannot unmarshal array").
	var obj struct {
		Slug               string          `json:"slug"`
		ProblemStatement   string          `json:"problem_statement"`
		AcceptanceCriteria json.RawMessage `json:"acceptance_criteria"`
		ContextRequired    json.RawMessage `json:"context_required"`
		Constraints        string          `json:"constraints"`
		Rationale          string          `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ChainTaskEntry{}, fmt.Errorf("full-object parse: %w", err)
	}
	if obj.Slug == "" {
		return ChainTaskEntry{}, fmt.Errorf("full-object entry: slug is required")
	}
	if strings.TrimSpace(obj.Rationale) == "" {
		return ChainTaskEntry{}, fmt.Errorf("full-object entry %q: rationale is required (the per-task 'why this task in this chain' grain)", obj.Slug)
	}
	ac, err := decodeStringOrList(obj.AcceptanceCriteria)
	if err != nil {
		return ChainTaskEntry{}, fmt.Errorf("full-object entry %q: acceptance_criteria: %w", obj.Slug, err)
	}
	cr, err := decodeStringOrList(obj.ContextRequired)
	if err != nil {
		return ChainTaskEntry{}, fmt.Errorf("full-object entry %q: context_required: %w", obj.Slug, err)
	}
	return ChainTaskEntry{
		Mode:               ChainTaskModeFull,
		Slug:               obj.Slug,
		ProblemStatement:   obj.ProblemStatement,
		AcceptanceCriteria: ac,
		// context_required is stored as a single joined string (the standalone task
		// row's form: StringField → FieldValue.AsJoined), so a list is joined the
		// SAME way the standalone forge(task) path joins it — preserving the parity
		// the chain `tasks` field documents.
		ContextRequired: fieldvalue.FieldValue{IsList: true, List: cr}.AsJoined(),
		Constraints:     obj.Constraints,
		Rationale:       obj.Rationale,
	}, nil
}

// decodeStringOrList accepts a string OR a []string OR null and returns the
// canonical []string form. Matches the underlying task schema's
// optional_string_or_list field type, so an embedded chain task accepts the same
// field shapes as standalone forge(task) (bug 1086). Used for both
// acceptance_criteria and context_required.
func decodeStringOrList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '[' {
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode list: %w", err)
		}
		return list, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode string: %w", err)
		}
		if s == "" {
			return nil, nil
		}
		return []string{s}, nil
	}
	return nil, fmt.Errorf("must be string or []string; got first byte %q", string(raw[0]))
}

// peelChainTasksFromParams extracts the raw `tasks` value from raw
// params, parses it via ParseChainTasks, and returns (entries, true,
// nil) if it was present. Returns (nil, false, nil) if the caller did
// not supply tasks. Returns a parse error when the field is present
// but malformed (missing rationale on a full-object entry, etc.).
//
// Mutates params: removes the `tasks` key on a successful parse so the
// caller can re-inject a synthesized pipe-delimited representation for
// the validator's eyes. Also probes the nested `fields:{...}` envelope
// in case the caller used the structured shape; same removal pattern.
func peelChainTasksFromParams(params map[string]json.RawMessage) ([]ChainTaskEntry, bool, error) {
	// Top-level sugar shape first.
	if raw, ok := params["tasks"]; ok {
		entries, err := ParseChainTasks(raw)
		if err != nil {
			return nil, false, err
		}
		delete(params, "tasks")
		return entries, true, nil
	}
	// Structured shape: peel from inside fields:{...}.
	if fieldsRaw, ok := params["fields"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(fieldsRaw, &nested); err != nil {
			return nil, false, nil // fall through to the normal validator path
		}
		if raw, hit := nested["tasks"]; hit {
			entries, err := ParseChainTasks(raw)
			if err != nil {
				return nil, false, err
			}
			delete(nested, "tasks")
			repacked, mErr := json.Marshal(nested)
			if mErr != nil {
				return nil, false, mErr
			}
			params["fields"] = repacked
			return entries, true, nil
		}
	}
	return nil, false, nil
}

// placeKeyInParams re-injects a synthesized value into params, honoring
// the envelope shape (top-level sugar vs nested fields:{...}). The
// caller peelChainTasksFromParams already chose which envelope owned
// the original tasks field; placeKeyInParams just round-trips back to
// the same slot so the validator sees a homogeneous string-list shape.
func placeKeyInParams(params map[string]json.RawMessage, key string, value json.RawMessage) {
	// If a fields:{...} envelope exists, re-inject there.
	if fieldsRaw, ok := params["fields"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(fieldsRaw, &nested); err == nil {
			nested[key] = value
			if repacked, mErr := json.Marshal(nested); mErr == nil {
				params["fields"] = repacked
				return
			}
		}
	}
	params[key] = value
}

// truncateTo returns the first n characters of s; or s if shorter. Used
// to synthesize a placeholder Scope from a full-object entry's
// problem_statement so the validator's pipe-delimited round-trip path
// has a non-empty middle segment. The synthesized value never reaches
// the live task row — the chain hook ignores Scope for full-object
// entries — but the validator sees a valid pipe-delimited string list.
func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ChainTasksMode returns the analytics-mode label for a ChainAndTasksForged
// event given a parsed entries slice. 'empty' when no entries; 'mixed'
// when both pipe + full present; otherwise the homogeneous mode.
func ChainTasksMode(entries []ChainTaskEntry) string {
	if len(entries) == 0 {
		return "empty"
	}
	sawPipe := false
	sawFull := false
	for _, e := range entries {
		switch e.Mode {
		case ChainTaskModePipe:
			sawPipe = true
		case ChainTaskModeFull:
			sawFull = true
		}
	}
	switch {
	case sawPipe && sawFull:
		return "mixed"
	case sawFull:
		return string(ChainTaskModeFull)
	default:
		return string(ChainTaskModePipe)
	}
}
