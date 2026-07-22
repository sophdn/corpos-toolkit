package actiondocs_test

// surface_contract_net_test.go consolidates the per-surface action-doc
// characterization-net TRIPLE — describe / admin-HTTP-payload / meta-tool
// description — into ONE table-driven test keyed by surface. It is the standing
// byte-parity oracle for every surface that has migrated onto the derive-contract
// (work / knowledge / measure / admin / ml). Each surface's docs are fixed by
// construction (the handler param structs + the co-located authored descriptors)
// plus these goldens.
//
// ── Why this file exists (suggestion 38 + bug 941) ──────────────────────────
// The five surfaces originally each carried a near-identical *_contract_net_test.go
// hand-rolling the same three assertions, differing only in the surface-name token
// (suggestion 38: "near-identical triples … the most literal example of parallel
// work that hasn't gelled"). They also each documented a per-surface regeneration
// command of the shape `-run TestContractNet_<Surface>` — which, because the
// substring `TestContractNet_<Surface>` is absent from the `Describe<Surface>` /
// `AdminActionDocs<Surface>` test names, silently regenerated only ONE of the three
// goldens and exited 0 (bug 941, hit live during measure's T1).
//
// Consolidation kills both: the triple lives ONCE as surface-parameterized helpers
// (assertSurfaceDescribeNet / assertSurfaceAdminPayloadNet / assertSurfaceDescriptionNet),
// each surface is a row in surfaceNets, and the subtest structure means the
// regeneration command scopes correctly by construction (see below). Work's
// genuinely surface-specific tests (work_actions + CallShape) stay in
// contract_net_test.go — only the shared triple consolidates here.
//
// ── Regeneration (the single correct command) ───────────────────────────────
// Regenerate ALL surface goldens:
//
//	UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run TestContractNet_Surfaces
//
// Scope to one surface (no footgun — the surface is a subtest, not a name substring):
//
//	UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run TestContractNet_Surfaces/measure
//
// Scope to one golden of one surface:
//
//	UPDATE_CONTRACT_NET=1 go test -tags sqlite_fts5 ./internal/actiondocs/ -run TestContractNet_Surfaces/measure/describe
//
// Under refactoring-discipline the goldens are the oracle: a byte difference is a
// DEFECT to fix, not a baseline to update. The only legitimate regenerations are
// the establishing T1 baselines and the enumerated, reviewed blessed-delta cells:
//
//	work      — batch.ops object→object[] (the single intended cell in the founding chain).
//	knowledge — curation_read/promote/reject.id string→integer + curation_bulk_action.filter
//	            string→object (four cells at the T3 flip).
//	measure   — bench_run.override_flags object→object[]; benchmark_record/query/replay
//	            param documentation (chain finalize-action-docs-epic T4, bug 940).
//	admin     — project_register/host_register/host_remove/host_list/remote_exec param
//	            documentation (chain finalize-action-docs-epic T4, bug 943). The founding
//	            admin migration moved no cell (its derive reproduced output exactly).
//	ml        — inference prose→structured [[params]]/[[errors]]/[returns] restructure
//	            (the T3 flip).
//
// write_actions on the /admin/action-docs payload is excluded for every surface:
// it is dispatch-policy-derived (action-manifests/dispatch-policy.toml), not
// action-doc-derived, so wiring the real policy file would couple this net to an
// unrelated artifact's churn. AppState is built with DispatchPolicyPath="" so
// write_actions is the empty map; the policy→write_actions projection is covered
// by observehttp/actiondocs_test.go.
//
// The net lives in the actiondocs package's external test scope (actiondocs_test)
// so it can import work + admin + observehttp without an import cycle (the real
// actiondocs package imports none of them). It shares contract_net_test.go's
// fixture helpers (netDir / compareOrUpdate / marshalIndent / embeddedReg).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"toolkit/internal/actiondocs"
	"toolkit/internal/admin"
	"toolkit/internal/observehttp"
	"toolkit/internal/testutil"
)

// surfaceNet is one row of the contract-net table: the action-doc surface name
// plus its eager meta-tool description constant (the <Surface>Description blurb
// the tool catalog emits, pinned so an accidental edit is caught).
type surfaceNet struct {
	surface     string
	description string
}

// surfaceNets is the authoritative table of action-doc surfaces under the
// derive-contract. Each surface contributes exactly three goldens —
// describe_<surface>.json, admin_action_docs_<surface>.json, <surface>_description.txt —
// under testdata/contract_net/. Adding a newly-migrated surface is one row here,
// not a new ~130-line file.
var surfaceNets = []surfaceNet{
	{"work", actiondocs.WorkDescription},
	{"knowledge", actiondocs.KnowledgeDescription},
	{"measure", actiondocs.MeasureDescription},
	{"admin", actiondocs.AdminDescription},
	{"ml", actiondocs.MLDescription},
	{"fs", actiondocs.FsDescription},
	{"sys", actiondocs.SysDescription},
}

// TestContractNet_Surfaces runs the shared describe/payload/description triple for
// every surface in surfaceNets. Each surface is a subtest and each of its three
// assertions is a nested subtest, so `-run TestContractNet_Surfaces/<surface>`
// scopes cleanly to one surface and a bare `-run TestContractNet_Surfaces`
// regenerates every surface golden — structurally dissolving bug 941's
// silent-partial-regen footgun.
func TestContractNet_Surfaces(t *testing.T) {
	for _, sn := range surfaceNets {
		sn := sn
		t.Run(sn.surface, func(t *testing.T) {
			t.Run("describe", func(t *testing.T) {
				assertSurfaceDescribeNet(t, sn.surface)
			})
			t.Run("admin_payload", func(t *testing.T) {
				assertSurfaceAdminPayloadNet(t, sn.surface)
			})
			t.Run("description", func(t *testing.T) {
				assertSurfaceDescriptionNet(t, sn.surface, sn.description)
			})
		})
	}
}

// assertSurfaceDescribeNet pins admin.action_describe(surface, X) for every doc on
// the surface (all real actions + the _general cross-cutting chunk), served from
// the embedded corpus exactly as production does. Golden: describe_<surface>.json.
func assertSurfaceDescribeNet(t *testing.T, surface string) {
	t.Helper()
	reg := embeddedReg(t)
	deps := admin.Deps{ActionDocs: reg}

	names := reg.Names(surface)
	names = append(names, actiondocs.GeneralAction)

	out := map[string]json.RawMessage{}
	for _, name := range names {
		params, _ := json.Marshal(map[string]string{"surface": surface, "action": name})
		res, err := admin.HandleActionDescribe(context.Background(), deps, params)
		if err != nil {
			t.Fatalf("HandleActionDescribe(%s, %s): %v", surface, name, err)
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal describe(%s, %s): %v", surface, name, err)
		}
		out[name] = raw
	}
	compareOrUpdate(t, "describe_"+surface+".json", marshalIndent(t, out))
}

// assertSurfaceAdminPayloadNet pins the GET /admin/action-docs?surface=<surface>
// payload (the dashboard ActionDocs page's data source). Served flagless from the
// embedded corpus (corpus_path == "embedded"); write_actions excluded by design
// (see the file header). Golden: admin_action_docs_<surface>.json.
func assertSurfaceAdminPayloadNet(t *testing.T, surface string) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	srv := httptest.NewServer(observehttp.BuildRouter(observehttp.AppState{
		Pool:       pool,
		ActionDocs: embeddedReg(t),
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/admin/action-docs?surface=" + surface)
	if err != nil {
		t.Fatalf("GET /admin/action-docs?surface=%s: %v", surface, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Re-marshal through a generic map so the snapshot is indented + key-sorted
	// (the handler emits compact JSON; we want a reviewable golden).
	var generic any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	compareOrUpdate(t, "admin_action_docs_"+surface+".json", marshalIndent(t, generic))
}

// assertSurfaceDescriptionNet pins the <Surface>Description meta-tool constant.
// Golden: <surface>_description.txt.
func assertSurfaceDescriptionNet(t *testing.T, surface, description string) {
	t.Helper()
	compareOrUpdate(t, surface+"_description.txt", []byte(description))
}
