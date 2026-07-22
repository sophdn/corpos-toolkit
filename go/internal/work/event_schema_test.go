package work_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/work"
)

func TestEventSchema_ListsRegisteredTypes(t *testing.T) {
	res, err := work.HandleEventSchema(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("event_schema list: %v", err)
	}
	if len(res.Types) == 0 {
		t.Fatal("expected a non-empty registered-type list")
	}
	// A few well-known types must be present.
	got := strings.Join(res.Types, ",")
	for _, want := range []string{"BugResolved", "ChainCreated", "TaskCreated"} {
		if !strings.Contains(got, want) {
			t.Fatalf("registered types missing %q; got %s", want, got)
		}
	}
}

func TestEventSchema_DescribesPayload(t *testing.T) {
	res, err := work.HandleEventSchema(context.Background(), json.RawMessage(`{"type":"BugResolved"}`))
	if err != nil {
		t.Fatalf("event_schema describe: %v", err)
	}
	if res.Type != "BugResolved" || len(res.Schema) == 0 {
		t.Fatalf("expected BugResolved schema, got %+v", res)
	}
	schema := string(res.Schema)
	// The payload schema should expose the discriminator + commit_sha fields.
	if !strings.Contains(schema, "kind") || !strings.Contains(schema, "commit_sha") {
		t.Fatalf("BugResolved schema missing expected fields: %s", schema)
	}
}

func TestEventSchema_UnknownTypeHints(t *testing.T) {
	res, err := work.HandleEventSchema(context.Background(), json.RawMessage(`{"type":"NoSuchEventType"}`))
	if err != nil {
		t.Fatalf("event_schema unknown: %v", err)
	}
	if res.Error == "" || !strings.Contains(res.Error, "unknown event type") {
		t.Fatalf("expected an unknown-type error with a hint, got %+v", res)
	}
}
