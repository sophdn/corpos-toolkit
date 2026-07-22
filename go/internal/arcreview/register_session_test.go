package arcreview

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHandleRegisterSession_UpsertsRow(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}

	res, err := HandleRegisterSession(context.Background(), deps, "corpos-toolkit",
		json.RawMessage(`{"session_id":"s1","transcript_path":"/tmp/t.jsonl"}`))
	if err != nil {
		t.Fatalf("register err: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok", res.Status)
	}

	var proj, tp string
	if err := pool.DB().QueryRow(
		`SELECT project_id, transcript_path FROM session_registry WHERE session_id='s1'`).
		Scan(&proj, &tp); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if proj != "corpos-toolkit" || tp != "/tmp/t.jsonl" {
		t.Errorf("row = (proj=%q, tp=%q), want (corpos-toolkit, /tmp/t.jsonl)", proj, tp)
	}

	// Re-register the same session with a new transcript → UPSERT updates in place.
	if _, err := HandleRegisterSession(context.Background(), deps, "corpos-toolkit",
		json.RawMessage(`{"session_id":"s1","transcript_path":"/tmp/new.jsonl"}`)); err != nil {
		t.Fatalf("upsert err: %v", err)
	}
	var n int
	if err := pool.DB().QueryRow(
		`SELECT count(*), max(transcript_path) FROM session_registry WHERE session_id='s1'`).
		Scan(&n, &tp); err != nil {
		t.Fatalf("scan2: %v", err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 row after upsert, got %d", n)
	}
	if tp != "/tmp/new.jsonl" {
		t.Errorf("upsert did not update transcript_path: got %q", tp)
	}
}

func TestHandleRegisterSession_Validation(t *testing.T) {
	pool := openTestPool(t)
	deps := Deps{Pool: pool}
	for _, c := range []struct{ name, params string }{
		{"missing session_id", `{"transcript_path":"/tmp/t.jsonl"}`},
		{"missing transcript_path", `{"session_id":"s1"}`},
		{"blank session_id", `{"session_id":"   ","transcript_path":"/tmp/t.jsonl"}`},
		{"blank transcript_path", `{"session_id":"s1","transcript_path":"  "}`},
		{"malformed json", `{not json`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := HandleRegisterSession(context.Background(), deps, "p",
				json.RawMessage(c.params)); err == nil {
				t.Errorf("expected an error for %s", c.name)
			}
		})
	}
}
