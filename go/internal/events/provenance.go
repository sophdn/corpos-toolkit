package events

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Provenance guard — stops a write from recording an agent's OWN scope/deferral
// decision as the user's.
//
// WHY THIS EXISTS: a recurring failure where an agent paraphrases its own
// scoping choice ("I'll do the minimal version") into a persisted record as a
// user decision ("the user chose to defer the full port in the 2026-06-26 scope
// decision"). A later session reads that record and treats the laundered claim
// as settled user intent — false provenance that immunizes an agent decision
// against scrutiny. The companion gradient-question-guard hook removes the main
// GENERATOR of this (the completeness-gradient menu); this guard catches the
// residual at the write path itself.
//
// It runs at the single shared event-emit chokepoints ([prepareRecord] for the
// record / forge-sugar path, [Emit] for the legacy typed-forge path), so every
// event type that carries free text is covered at one point. A trip returns
// *ErrInvalidInput, which the record surface turns into a per-event rejection
// (a ghost) and the legacy forge path surfaces as a hard error — in both cases
// the agent gets the reason and must reword or cite.
//
// SCOPE — deliberately narrow to keep false positives near zero: it fires only
// on a user DECISION verb (chose/decided/agreed/…) adjacent to a REDUCTION word
// (defer/minimal/skip/scope/…), or "per the user's … decision", or
// "user-approved … scope". Plain "the user requested X" / "the user asked for a
// minimal version" is a request, not a laundered decision, and does NOT trip.
//
// LIMIT — it cannot verify a citation is real (an agent could quote fabricated
// words), and an unrelated quote elsewhere in the text satisfies the citation
// check. It targets the observed shape: a BARE decision-attribution with no
// citation at all (exactly the landmine). The stronger version is a typed
// provenance field on the record schema; this is the buildable-now guard.

// userScopeDecisionRX: "(the user|you) <decision-verb> … <reduction-word>".
var userScopeDecisionRX = regexp.MustCompile(`(?i)\b(the user|you)\b[^.!?\n]{0,40}\b(chose|picked|selected|decided|elected|opted|agreed|approved)\b[^.!?\n]{0,45}\b(defer|skip|cut|de-?scope|scoped?|scoping|dropp?ed?|drop|leave|hide|minimal|partial|stop|punt|postpone|narrow|less)\b`)

// perUserDecisionRX: "per the user's … decision".
var perUserDecisionRX = regexp.MustCompile(`(?i)\bper (the )?user'?s\b[^.!?\n]{0,60}\bdecision\b`)

// userApprovedScopeRX: "user-approved … <scope/reduction word>".
var userApprovedScopeRX = regexp.MustCompile(`(?i)\buser-?approved\b[^.!?\n]{0,45}\b(scope|defer|cut|skip|minimal|narrow|partial|drop)\b`)

// citationRX: a quoted span of the user's words, or an explicit "you said/…"
// reference. A bare date is NOT a citation — a fabricated "<date> scope
// decision" is precisely the failure being guarded against.
var citationRX = regexp.MustCompile(`(?i)("[^"]{6,}"|'[^']{6,}'|\byou (said|wrote|told|asked|requested|directed|instructed)\b|\byour (message|words|directive|prompt|reply|request)\b)`)

func attributesUserScopeDecision(s string) bool {
	return userScopeDecisionRX.MatchString(s) ||
		perUserDecisionRX.MatchString(s) ||
		userApprovedScopeRX.MatchString(s)
}

func provenanceCited(s string) bool { return citationRX.MatchString(s) }

// checkProvenanceText is the per-field rule: a scope/deferral decision attributed
// to the user must cite what the user actually said.
func checkProvenanceText(field, text string) error {
	if attributesUserScopeDecision(text) && !provenanceCited(text) {
		return &ErrInvalidInput{
			Field: field,
			Reason: "provenance: this text attributes a scope/deferral DECISION to the user without citing what the user actually said. " +
				"If the user directed it, quote their words (e.g. you said: \"…\"). If it was YOUR decision, write it as agent-attributed " +
				"(e.g. \"I scoped this to minimal; the full port is deferred\"). Recording an agent scope decision as the user's — especially " +
				"with a bare \"<date> scope decision\" — is the exact laundering this guard prevents.",
		}
	}
	return nil
}

// checkPayloadProvenance walks every string in an event payload and applies the
// per-field rule, covering all event types at one point. A malformed payload is
// not this guard's concern — the schema validator rejects those — so it is
// silently skipped here.
func checkPayloadProvenance(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil
	}
	return walkProvenance("payload", v)
}

func walkProvenance(path string, v any) error {
	switch t := v.(type) {
	case string:
		return checkProvenanceText(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic field order → deterministic error
		for _, k := range keys {
			if err := walkProvenance(path+"."+k, t[k]); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range t {
			if err := walkProvenance(fmt.Sprintf("%s[%d]", path, i), e); err != nil {
				return err
			}
		}
	}
	return nil
}
