package work

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// listParamSet constrains decodeListParams to the concrete compact-list param
// structs (not bare `any`, which the forbidigo rule restricts to the
// internal/db + internal/dispatch boundaries). Add a struct here when a new
// list handler adopts strict decoding.
type listParamSet interface {
	bugListParams | suggestionListParams | taskListParams
}

// decodeListParams strict-decodes a compact-list action's params: any key the
// target struct does not define is rejected instead of being silently dropped.
//
// Silent drops were a footgun — an unknown filter like `pattern` (bug_list has
// no such field) was discarded, so the full unfiltered list returned, looking
// filtered/exhaustive. This is the read/list complement to the dispatcher's
// envelope-repair guards (bug 1070 for project, bug 1403 for rationale).
//
// One key is deliberately tolerated: a `project` nested in params. The
// dispatcher's bug-1070 repair promotes it to the envelope-level project (the
// authoritative scope) but leaves the key in params, so we drop it here before
// the strict pass — otherwise the just-repaired call would be rejected for
// carrying an "unknown" field. Every OTHER unrecognised key, including a
// misplaced `cwd`, still errors with a hint.
func decodeListParams[T listParamSet](params json.RawMessage, action, acceptedHint string) (T, error) {
	var p T
	if len(params) == 0 {
		return p, nil
	}
	cleaned, err := dropEnvelopeProject(params)
	if err != nil {
		return p, fmt.Errorf("parse params: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(cleaned))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, mapUnknownParamErr(err, action, acceptedHint)
	}
	return p, nil
}

// dropEnvelopeProject removes a `project` key from a params object — the
// dispatcher's bug-1070 repair already consumed it into the envelope. Non-
// object or projectless params pass through untouched (the strict decoder then
// surfaces any structural problem).
func dropEnvelopeProject(params json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil {
		return params, nil
	}
	if _, ok := obj["project"]; !ok {
		return params, nil
	}
	delete(obj, "project")
	return json.Marshal(obj)
}

// mapUnknownParamErr turns the standard library's terse `json: unknown field
// "x"` into an actionable message. A misplaced `cwd` (an envelope-level field)
// gets a "move it next to action" hint; every other unknown key names the
// filters the action accepts. Non-unknown-field decode errors pass through.
func mapUnknownParamErr(err error, action, acceptedHint string) error {
	const pfx = "json: unknown field "
	msg := err.Error()
	i := strings.Index(msg, pfx)
	if i < 0 {
		return fmt.Errorf("parse params: %w", err)
	}
	field := strings.Trim(strings.TrimSpace(msg[i+len(pfx):]), `"`)
	if field == "cwd" {
		return fmt.Errorf(
			"%q is an envelope-level field, not a %s param: pass it next to `action` (top level), not inside `params`",
			field, action)
	}
	return fmt.Errorf("unknown %s param %q; accepted: %s", action, field, acceptedHint)
}
