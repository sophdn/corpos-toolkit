package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAcceptance_ReadSymbolWrongType is the PRINCIPAL-OWNED acceptance test for bug 1020
// (corpos verification-integrity rehearsal, chain 366 T8). It is authored by the principal
// and is the authoritative gate — the worker must NOT author or edit it.
//
// Bug: fs.read with a non-string `symbol` param raw-crashes via ReadParams.UnmarshalJSON
// with "json: cannot unmarshal bool into Go struct field alias.symbol of type string" — a
// tool-defect-shaped failure rather than an actionable usage hint.
//
// This test is deliberately NON-TAUTOLOGICAL: a raw crash (err != nil) does NOT satisfy it.
// A graceful outcome is required — EITHER a clear error that names the `symbol` param and
// does NOT leak the raw "cannot unmarshal" text, OR successful coercion/ignore (err == nil).
// On the unfixed tree it FAILS (the raw unmarshal error leaks); a real fix makes it pass.
func TestAcceptance_ReadSymbolWrongType(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.go")
	if err := os.WriteFile(fp, []byte("package x\n\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	params := []byte(fmt.Sprintf(`{"file_path":%q,"symbol":true}`, fp))

	_, err := HandleRead(context.Background(), params)
	if err == nil {
		return // coercion / ignore path — a graceful outcome, acceptable.
	}
	msg := err.Error()
	if strings.Contains(msg, "cannot unmarshal") {
		t.Fatalf("bug 1020 unfixed: raw json unmarshal error leaked to the agent: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "symbol") {
		t.Fatalf("a wrong-typed symbol error must name the offending `symbol` param, got: %v", err)
	}
}
