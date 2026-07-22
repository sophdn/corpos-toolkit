package actiondocs_test

// contract_net_test.go carries the WORK-SPECIFIC half of the action-docs
// characterization net plus the fixture helpers shared by every surface's net.
//
// The describe / admin-HTTP-payload / meta-tool-description triple that EVERY
// migrated surface shares (work included) lives in surface_contract_net_test.go
// as one table-driven test (TestContractNet_Surfaces). What stays here is the two
// outputs unique to work — it is the only surface with a work_actions catalog and
// a CallShape/IdentifierRequiredError renderer:
//
//	1. work_actions  — the full WorkActionsResult catalog (HandleWorkActions),
//	                   pinned in declaration order (grouped chains→tasks→bugs→…).
//	2. call_shapes   — CallShape + IdentifierRequiredError for EVERY catalog action
//	                   (the identify actions are the load-bearing subset; capturing
//	                   all of them is cheap and pins the renderer wholesale).
//
// plus the helpers (netDir / compareOrUpdate / marshalIndent / embeddedReg) the
// surface table imports.
//
// The net lives in the actiondocs package's external test scope so it can import
// work + admin + observehttp without an import cycle (the real actiondocs package
// imports none of them).
//
// Regenerating the work-specific goldens:
//
//	UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run 'TestContractNet_(WorkActions|CallShapes)'
//
// (The shared triple — including work's describe/payload/description — regenerates
// via `-run TestContractNet_Surfaces`; see surface_contract_net_test.go.) Under
// refactoring-discipline the goldens are the oracle: a byte difference is a DEFECT
// to fix, not a baseline to update. The only legitimate work regenerations have
// already happened: T1 (establishing the net) and the single enumerated batch.ops
// object→object[] cell in the founding chain's T4.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/work"
)

// netDir is the golden-fixture directory beside this test file.
func netDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot resolve testdata path")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "contract_net")
}

// compareOrUpdate writes the golden when UPDATE_CONTRACT_NET is set, else asserts
// the captured bytes equal the committed golden byte-for-byte.
func compareOrUpdate(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(netDir(t), name)
	if os.Getenv("UPDATE_CONTRACT_NET") != "" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		t.Logf("updated golden %s (%d bytes)", name, len(got))
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run UPDATE_CONTRACT_NET=1 to generate)", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("contract net DRIFT in %s: captured output differs from the committed golden.\n"+
			"This is the parity oracle — a difference here means an observable action-docs\n"+
			"output changed. If the change was unintended, FIX the producer. If the baseline\n"+
			"legitimately moved (an enumerated, reviewed blessed delta), regenerate with\n"+
			"UPDATE_CONTRACT_NET=1 and review the diff.", name)
	}
}

// marshalIndent produces stable, reviewable JSON (encoding/json sorts map keys,
// so map-backed snapshots are deterministic).
func marshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}

// embeddedReg loads the production embedded corpus (what flagless stdio + the
// HTTP daemon both serve). Shared by every surface's describe/payload net.
func embeddedReg(t *testing.T) *actiondocs.Registry {
	t.Helper()
	reg, err := actiondocs.LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	return reg
}

// catalogNames returns the work catalog action names in declaration order.
func catalogNames(t *testing.T) []string {
	t.Helper()
	res, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("HandleWorkActions: %v", err)
	}
	names := make([]string, 0, len(res))
	for _, s := range res {
		names = append(names, s.Name)
	}
	return names
}

// TestContractNet_WorkActions pins the full work_actions catalog.
func TestContractNet_WorkActions(t *testing.T) {
	res, err := work.HandleWorkActions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("HandleWorkActions: %v", err)
	}
	compareOrUpdate(t, "work_actions.json", marshalIndent(t, res))
}

// TestContractNet_CallShapes pins CallShape + IdentifierRequiredError for every
// catalog action. The identify actions (id-or-slug rows) are the subset the AC
// names; capturing all actions pins the renderer's full surface.
func TestContractNet_CallShapes(t *testing.T) {
	type shape struct {
		CallShape          string `json:"call_shape"`
		IdentifierRequired string `json:"identifier_required_error"`
	}
	out := map[string]shape{}
	for _, name := range catalogNames(t) {
		out[name] = shape{
			CallShape:          work.CallShape(name),
			IdentifierRequired: work.IdentifierRequiredError(name),
		}
	}
	compareOrUpdate(t, "call_shapes.json", marshalIndent(t, out))
}
