package knowledge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/router"
	"toolkit/internal/testutil"
)

const handlerTestProject = "test-proj"

func libDeps(t *testing.T, llamaURL string) Deps {
	t.Helper()
	r := router.NewWithClients(llamacpp.New(llamaURL), nil, "qwen2.5-32b")
	return Deps{Pool: testutil.NewTestDB(t), Router: r}
}

// ── HandleLibraryAdd / HandleLibraryGet ────────────────────────────

func TestHandleLibraryAdd_ProjectRequired(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	resp, _ := HandleLibraryAdd(context.Background(), deps, "", json.RawMessage(`{}`))
	if !strings.Contains(resp.Error, "project is required") {
		t.Errorf("expected project-required, got %q", resp.Error)
	}
}

func TestHandleLibraryAdd_FlatFieldsHappyPath(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{
		"dewey":           "500.42",
		"primary_author":  "Doe",
		"year":            2025,
		"citation_raw":    "Doe, J. (2025). Foo.",
		"establishes":     "establishes baseline",
		"what_it_answers": "what is foo",
		"invoke_when":     "when",
		"tags":            []string{"foo"},
	})
	resp, err := HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !resp.OK || resp.Dewey != "500.42" {
		t.Errorf("add response: %+v", resp)
	}
	// Roundtrip read.
	getParams, _ := json.Marshal(map[string]any{"dewey": "500.42"})
	got, _ := HandleLibraryGet(context.Background(), deps, handlerTestProject, getParams)
	if got.Entry == nil {
		t.Fatalf("get returned no entry: %+v", got)
	}
	if got.Entry.Citation.PrimaryAuthor != "Doe" {
		t.Errorf("primary_author: %s", got.Entry.Citation.PrimaryAuthor)
	}
}

func TestHandleLibraryAdd_RejectsMissingRequired(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"dewey": "500.42"})
	resp, _ := HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	if !strings.Contains(resp.Error, "primary_author is required") {
		t.Errorf("expected primary_author error, got %q", resp.Error)
	}
}

func TestHandleLibraryGet_NotFoundReturnsTypedError(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"dewey": "999"})
	resp, _ := HandleLibraryGet(context.Background(), deps, handlerTestProject, params)
	if resp.Error != "not_found" || resp.Dewey != "999" {
		t.Errorf("expected typed not_found, got %+v", resp)
	}
}

// ── HandleLibraryUpdate / HandleLibraryRetire ──────────────────────

func TestHandleLibraryUpdate_PartialPayload(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	addParams, _ := json.Marshal(map[string]any{
		"dewey": "500.42", "primary_author": "Doe", "citation_raw": "c",
		"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
	})
	_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, addParams)
	updateParams, _ := json.Marshal(map[string]any{
		"dewey":  "500.42",
		"update": map[string]any{"establishes": "revised"},
	})
	resp, _ := HandleLibraryUpdate(context.Background(), deps, handlerTestProject, updateParams)
	if resp.Result == nil {
		t.Fatalf("update returned no result: %+v", resp)
	}
	if len(resp.Result.FieldsChanged) != 1 || resp.Result.FieldsChanged[0] != "establishes" {
		t.Errorf("fields_changed: %v", resp.Result.FieldsChanged)
	}
}

func TestHandleLibraryUpdate_MissingUpdateBlock(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"dewey": "500.42"})
	resp, _ := HandleLibraryUpdate(context.Background(), deps, handlerTestProject, params)
	if !strings.Contains(resp.Error, "params.update is required") {
		t.Errorf("expected update-required, got %q", resp.Error)
	}
}

func TestHandleLibraryRetire_HappyPath(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	addParams, _ := json.Marshal(map[string]any{
		"dewey": "500.42", "primary_author": "Doe", "citation_raw": "c",
		"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
	})
	_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, addParams)
	retireParams, _ := json.Marshal(map[string]any{"dewey": "500.42", "reason": "obsolete"})
	resp, _ := HandleLibraryRetire(context.Background(), deps, handlerTestProject, retireParams)
	if !resp.OK {
		t.Errorf("retire: %+v", resp)
	}
}

// ── HandleLibraryListActive / list_sections / list_dewey ───────────

func TestHandleLibraryListActive_ReturnsEntries(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	for _, d := range []string{"500.1", "500.2"} {
		params, _ := json.Marshal(map[string]any{
			"dewey": d, "primary_author": "Doe", "citation_raw": "c",
			"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
		})
		_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	}
	resp, _ := HandleLibraryListActive(context.Background(), deps, handlerTestProject, nil)
	if len(resp.Entries) != 2 {
		t.Errorf("expected 2, got %d", len(resp.Entries))
	}
}

func TestHandleLibraryListDewey_FiltersByPrefix(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	for _, d := range []string{"500.1", "510.5"} {
		params, _ := json.Marshal(map[string]any{
			"dewey": d, "primary_author": "Doe", "citation_raw": "c",
			"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
		})
		_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	}
	params, _ := json.Marshal(map[string]any{"prefix": "500"})
	resp, _ := HandleLibraryListDewey(context.Background(), deps, handlerTestProject, params)
	if resp.Count != 1 || resp.Prefix != "500" {
		t.Errorf("list_dewey: %+v", resp)
	}
}

// ── HandleLibraryFind ──────────────────────────────────────────────

func TestHandleLibraryFind_KeywordModeRequiresQuery(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"mode": "keyword"})
	resp, _ := HandleLibraryFind(context.Background(), deps, handlerTestProject, params)
	if !strings.Contains(resp.Error, "keyword mode requires params.query") {
		t.Errorf("expected query-required, got %q", resp.Error)
	}
}

func TestHandleLibraryFind_KeywordMatches(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	addParams, _ := json.Marshal(map[string]any{
		"dewey": "500.42", "primary_author": "Doe", "citation_raw": "c",
		"establishes":     "Establishes a cromulent baseline.",
		"what_it_answers": "w", "invoke_when": "i",
	})
	_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, addParams)
	findParams, _ := json.Marshal(map[string]any{"mode": "keyword", "query": "cromulent"})
	resp, _ := HandleLibraryFind(context.Background(), deps, handlerTestProject, findParams)
	if resp.Mode != "keyword" {
		t.Errorf("mode: %s (error=%q)", resp.Mode, resp.Error)
	}
}

// Parity-pin: semantic-mode rerank via Qwen must use lib/<dewey> path prefixing
// so the retrieve-response parser doesn't strip the leading-digit dewey. When
// Qwen returns paths with the lib/ prefix, qwenretrieve matches them and
// returns reranked entries with qwen_fell_back=false.
func TestHandleLibraryFind_SemanticRerankPrefixesDewey(t *testing.T) {
	// Qwen returns reranked paths "lib/500.2\nlib/500.1" — beta first.
	llamaSrv, _ := mockLlama(t, []string{"lib/500.2\nlib/500.1"})
	deps := libDeps(t, llamaSrv.URL)
	for _, d := range []string{"500.1", "500.2"} {
		params, _ := json.Marshal(map[string]any{
			"dewey": d, "primary_author": "Doe", "citation_raw": "c",
			"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
			"index_pointers": []map[string]any{{
				"section": "FoundationalDecisions", "question": "q", "role": "primary",
			}},
		})
		_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	}
	findParams, _ := json.Marshal(map[string]any{
		"mode": "semantic", "section": "FoundationalDecisions", "query": "rerank target",
	})
	resp, err := HandleLibraryFind(context.Background(), deps, handlerTestProject, findParams)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if resp.Mode != "semantic" {
		t.Fatalf("expected semantic mode, got %q (error=%q)", resp.Mode, resp.Error)
	}
	if resp.QwenFellBack {
		t.Errorf("expected successful rerank, qwen_fell_back=%v", resp.QwenFellBack)
	}
	if len(resp.SemanticResults) != 2 {
		t.Fatalf("results: %+v", resp.SemanticResults)
	}
	if resp.SemanticResults[0].Dewey != "500.2" {
		t.Errorf("expected 500.2 first after rerank, got %s", resp.SemanticResults[0].Dewey)
	}
}

func TestHandleLibraryFind_SemanticFallsBackWhenNoQuery(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{
		"dewey": "500.1", "primary_author": "Doe", "citation_raw": "c",
		"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
		"index_pointers": []map[string]any{{"section": "S", "question": "q", "role": "primary"}},
	})
	_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	findParams, _ := json.Marshal(map[string]any{"mode": "semantic", "section": "S"})
	resp, _ := HandleLibraryFind(context.Background(), deps, handlerTestProject, findParams)
	if resp.Mode != "semantic" {
		t.Errorf("mode: %s (error=%q)", resp.Mode, resp.Error)
	}
}

func TestHandleLibraryFind_RejectsUnknownMode(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	params, _ := json.Marshal(map[string]any{"mode": "wibble"})
	resp, _ := HandleLibraryFind(context.Background(), deps, handlerTestProject, params)
	if !strings.Contains(resp.Error, "keyword|semantic|manifest") {
		t.Errorf("unexpected error: %q", resp.Error)
	}
}

// ── HandleLibraryCrossReference ─────────────────────────────────────

func TestHandleLibraryCrossReference_DefaultSectionMode(t *testing.T) {
	deps := libDeps(t, "http://127.0.0.1:1")
	for _, d := range []string{"500.1", "500.2"} {
		params, _ := json.Marshal(map[string]any{
			"dewey": d, "primary_author": "Doe", "citation_raw": "c",
			"establishes": "e", "what_it_answers": "w", "invoke_when": "i",
			"index_pointers": []map[string]any{{
				"section": "S", "question": "q-" + d, "role": "primary",
			}},
		})
		_, _ = HandleLibraryAdd(context.Background(), deps, handlerTestProject, params)
	}
	xrParams, _ := json.Marshal(map[string]any{"dewey": "500.1"})
	resp, err := HandleLibraryCrossReference(context.Background(), deps, handlerTestProject, xrParams)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Result == nil {
		t.Fatalf("expected CrossRefResult, got error %q", resp.Error)
	}
	if len(resp.Result.BySection["S"]) != 1 || resp.Result.BySection["S"][0].Dewey != "500.2" {
		t.Errorf("expected 500.2 in section S, got %v", resp.Result.BySection)
	}
}
