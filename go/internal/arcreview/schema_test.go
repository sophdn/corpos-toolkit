package arcreview

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateDecision_NothingToFile_PayloadOptional(t *testing.T) {
	d := FilingDecision{
		Action:     ActionNothingToFile,
		Confidence: 0.3,
		Reasoning:  "session ran on rails",
	}
	if err := ValidateDecision(d); err != nil {
		t.Fatalf("nothing_to_file with nil payload should validate, got %v", err)
	}
	d.Payload = json.RawMessage("null")
	if err := ValidateDecision(d); err != nil {
		t.Fatalf("nothing_to_file with explicit null payload should validate, got %v", err)
	}
}

func TestValidateDecision_UnknownActionRejected(t *testing.T) {
	d := FilingDecision{Action: "summarize", Confidence: 0.5}
	err := ValidateDecision(d)
	var typed *ErrUnknownAction
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrUnknownAction, got %T (%v)", err, err)
	}
	if typed.Action != "summarize" {
		t.Fatalf("expected action 'summarize' in error, got %q", typed.Action)
	}
}

func TestValidateDecision_ConfidenceRange(t *testing.T) {
	for _, c := range []float64{-0.1, 1.5} {
		d := FilingDecision{
			Action:     ActionNothingToFile,
			Confidence: c,
		}
		err := ValidateDecision(d)
		var typed *ErrInvalidConfidence
		if !errors.As(err, &typed) {
			t.Fatalf("confidence %g: expected *ErrInvalidConfidence, got %T (%v)", c, err, err)
		}
	}
	// Boundary values inclusive.
	for _, c := range []float64{0.0, 1.0} {
		d := FilingDecision{
			Action:     ActionNothingToFile,
			Confidence: c,
		}
		if err := ValidateDecision(d); err != nil {
			t.Fatalf("confidence %g should validate at boundary, got %v", c, err)
		}
	}
}

func TestValidateDecision_MissingPayload(t *testing.T) {
	d := FilingDecision{Action: ActionForgeBug, Confidence: 0.9}
	err := ValidateDecision(d)
	var typed *ErrMissingPayload
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrMissingPayload, got %T (%v)", err, err)
	}
	d.Payload = json.RawMessage("null")
	err = ValidateDecision(d)
	if !errors.As(err, &typed) {
		t.Fatalf("explicit null payload: expected *ErrMissingPayload, got %T (%v)", err, err)
	}
}

func TestValidateDecision_ForgeBugRequiredFields(t *testing.T) {
	// Missing title.
	body, _ := json.Marshal(ForgeBugPayload{ProblemStatement: "p"})
	d := FilingDecision{Action: ActionForgeBug, Confidence: 0.9, Payload: body}
	err := ValidateDecision(d)
	var typed *ErrInvalidPayloadField
	if !errors.As(err, &typed) || typed.Field != "title" {
		t.Fatalf("expected *ErrInvalidPayloadField on title, got %T (%v)", err, err)
	}

	// Missing problem_statement.
	body, _ = json.Marshal(ForgeBugPayload{Title: "t"})
	d.Payload = body
	err = ValidateDecision(d)
	if !errors.As(err, &typed) || typed.Field != "problem_statement" {
		t.Fatalf("expected *ErrInvalidPayloadField on problem_statement, got %T (%v)", err, err)
	}

	// Bad severity.
	body, _ = json.Marshal(ForgeBugPayload{
		Title: "t", ProblemStatement: "p", Severity: "critical",
	})
	d.Payload = body
	err = ValidateDecision(d)
	if !errors.As(err, &typed) || typed.Field != "severity" {
		t.Fatalf("expected *ErrInvalidPayloadField on severity, got %T (%v)", err, err)
	}

	// Happy path.
	body, _ = json.Marshal(ForgeBugPayload{
		Title: "t", ProblemStatement: "p", Severity: "medium", Surface: "arcreview",
	})
	d.Payload = body
	if err := ValidateDecision(d); err != nil {
		t.Fatalf("forge_bug happy path should validate, got %v", err)
	}
}

func TestValidateDecision_VaultNoteKindClosed(t *testing.T) {
	body, _ := json.Marshal(ForgeVaultNotePayload{
		NoteKind: "scratch", Title: "t", Body: "b",
	})
	d := FilingDecision{Action: ActionForgeVaultNote, Confidence: 0.7, Payload: body}
	err := ValidateDecision(d)
	var typed *ErrInvalidPayloadField
	if !errors.As(err, &typed) || typed.Field != "note_kind" {
		t.Fatalf("expected *ErrInvalidPayloadField on note_kind, got %T (%v)", err, err)
	}

	body, _ = json.Marshal(ForgeVaultNotePayload{
		NoteKind: "decision", Title: "t", Body: "b",
	})
	d.Payload = body
	if err := ValidateDecision(d); err != nil {
		t.Fatalf("vault note happy path should validate, got %v", err)
	}
}

func TestValidateDecision_SkillUpdatePatchKindClosed(t *testing.T) {
	body, _ := json.Marshal(SkillUpdatePayload{
		SkillSlug: "x", PatchKind: "delete_section", Content: "c",
	})
	d := FilingDecision{Action: ActionSkillUpdate, Confidence: 0.8, Payload: body}
	err := ValidateDecision(d)
	var typed *ErrInvalidPayloadField
	if !errors.As(err, &typed) || typed.Field != "patch_kind" {
		t.Fatalf("expected *ErrInvalidPayloadField on patch_kind, got %T (%v)", err, err)
	}
}

func TestValidateDecision_MemoryWriteKindClosed(t *testing.T) {
	body, _ := json.Marshal(MemoryWritePayload{
		MemoryKind: "fact", Name: "n", Description: "d", Body: "b",
	})
	d := FilingDecision{Action: ActionMemoryWrite, Confidence: 0.6, Payload: body}
	err := ValidateDecision(d)
	var typed *ErrInvalidPayloadField
	if !errors.As(err, &typed) || typed.Field != "memory_kind" {
		t.Fatalf("expected *ErrInvalidPayloadField on memory_kind, got %T (%v)", err, err)
	}
}

func TestValidateDecision_MalformedJSON(t *testing.T) {
	d := FilingDecision{
		Action:     ActionForgeBug,
		Confidence: 0.9,
		Payload:    json.RawMessage("{not json"),
	}
	err := ValidateDecision(d)
	var typed *ErrInvalidPayloadShape
	if !errors.As(err, &typed) {
		t.Fatalf("expected *ErrInvalidPayloadShape, got %T (%v)", err, err)
	}
	if typed.Action != string(ActionForgeBug) {
		t.Fatalf("expected action forge_bug, got %q", typed.Action)
	}
	if typed.Err == nil {
		t.Fatalf("expected wrapped error in ErrInvalidPayloadShape")
	}
}
