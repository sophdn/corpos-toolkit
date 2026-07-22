package observehttp

import (
	"net/http"
	"sort"

	"toolkit/internal/actiondocs"
	"toolkit/internal/dispatch/policy"
)

// action-docs-corpus-frontend AF2: dashboard reader for the per-action
// documentation corpus.
//
// The admin endpoint surfaces the parsed corpus so operators and human
// collaborators can browse "what does forge_edit accept on a chain"
// without grepping main.go's TOC or reading TOMLs by hand. The chunks
// are baked into the binary via go:embed (source under
// go/internal/actiondocs/corpus/<surface>/<action>.toml); the in-process
// registry is shared with admin.action_describe (MCP).
//
// Caching: serves the startup-loaded registry by default; ?reload=1
// triggers a fresh load for that response (no shared-state swap — each
// operator reload independently re-reads). The reload re-reads the
// embedded corpus by default, or the on-disk override dir when
// --action-docs-dir is set. For MCP-side to also pick up corpus changes,
// the operator calls admin.schema_reload — same affordance as for
// dispatch-policy.
//
// See docs/ACTION_DOCS_FRONTEND.md for the full design.

// actionDocsResponse is the wire shape for GET /admin/action-docs. The
// nested ActionDoc shape is reused verbatim from internal/actiondocs;
// JSON tags on actiondocs.ActionDoc preserve TOML field names so the
// disk-shape and the wire-shape are one schema.
type actionDocsResponse struct {
	Count        int                                         `json:"count"`
	Surfaces     []string                                    `json:"surfaces"`
	Actions      map[string]map[string]*actiondocs.ActionDoc `json:"actions"`
	WriteActions map[string]bool                             `json:"write_actions"`
	CorpusPath   string                                      `json:"corpus_path"`
	ParseErrors  []actionDocsParseError                      `json:"parse_errors"`
}

type actionDocsParseError struct {
	SourceFile string `json:"source_file"`
	Err        string `json:"err"`
}

// actionDocs is the GET /admin/action-docs handler.
//
// Failure modes:
//   - Registry is nil (corpus not loaded) → 200 with zero-value response.
//     Same degraded-but-rendering shape admin.action_describe (MCP)
//     uses when ActionDocs is nil.
//   - ?action= supplied without ?surface= → 400. The (surface, action)
//     pair is how chunks are keyed; a bare action filter is ambiguous.
//   - ?reload=1 with a load error → 500. Unlike a normal serve which
//     degrades to empty, an explicit reload that fails is signal worth
//     surfacing.
//   - Dispatch policy file absent/unloaded → response still 200; the
//     write_actions map is empty. The dashboard degrades to "kind:
//     unknown" rather than failing the render.
func (s AppState) actionDocs(w http.ResponseWriter, r *http.Request) {
	surface := r.URL.Query().Get("surface")
	action := r.URL.Query().Get("action")
	reload := r.URL.Query().Get("reload") == "1"

	if action != "" && surface == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "action filter requires surface",
		})
		return
	}

	reg := s.ActionDocs
	if reload {
		fresh, err := s.reloadActionDocs()
		if err != nil {
			dbErr(w, err)
			return
		}
		reg = fresh
	}

	resp := actionDocsResponse{
		Surfaces:     []string{},
		Actions:      map[string]map[string]*actiondocs.ActionDoc{},
		WriteActions: map[string]bool{},
		CorpusPath:   s.actionDocsCorpusPath(),
		ParseErrors:  []actionDocsParseError{},
	}

	if reg != nil {
		populateActionDocsResponse(&resp, reg, surface, action)
	}

	// Enrich with write-action membership from dispatch-policy.toml.
	// Read fresh per request (matches /admin/dispatch-policy's
	// reload-on-demand behavior); missing/unloaded ⇒ empty map.
	if s.DispatchPolicyPath != "" {
		pol, err := policy.Load(s.DispatchPolicyPath)
		if err == nil {
			for _, key := range pol.Actions() {
				if pol.Gates(splitFirst(key)).RequiresRationale {
					resp.WriteActions[key] = true
				}
			}
		}
	}

	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// reloadActionDocs re-reads the corpus for the ?reload=1 path. An on-disk
// override dir (--action-docs-dir) re-reads that dir for dev/hot-reload;
// otherwise the embedded corpus is re-read (the production default). When
// the corpus was disabled at startup (ActionDocs nil, no override dir) the
// reload stays disabled — an empty registry, not a silent re-enable.
func (s AppState) reloadActionDocs() (*actiondocs.Registry, error) {
	switch {
	case s.ActionDocsDir != "":
		return actiondocs.Load(s.ActionDocsDir)
	case s.ActionDocs != nil:
		return actiondocs.LoadEmbedded()
	default:
		return actiondocs.New(), nil
	}
}

// actionDocsCorpusPath labels the corpus source for the response: the
// override dir when set, "embedded" when served from the binary, "" when
// the corpus is disabled. Distinguishes embedded from disabled via the
// startup registry (nil ⟺ disabled — the same signal admin.action_describe
// degrades on).
func (s AppState) actionDocsCorpusPath() string {
	switch {
	case s.ActionDocsDir != "":
		return s.ActionDocsDir
	case s.ActionDocs != nil:
		return "embedded"
	default:
		return ""
	}
}

// populateActionDocsResponse copies docs from reg into resp, applying
// the surface/action filters. Filters are convenience for external
// callers (curl, scripts); the dashboard always fetches the full
// corpus on mount and filters client-side.
func populateActionDocsResponse(resp *actionDocsResponse, reg *actiondocs.Registry, surface, action string) {
	allSurfaces := reg.Surfaces()
	for _, sName := range allSurfaces {
		if surface != "" && sName != surface {
			continue
		}
		resp.Surfaces = append(resp.Surfaces, sName)
		actions := map[string]*actiondocs.ActionDoc{}

		// Real actions + _general — Names() excludes _general, so fetch
		// it explicitly. The list view filters _general out client-side;
		// the detail view renders it.
		names := reg.Names(sName)
		if general, ok := reg.Get(sName, actiondocs.GeneralAction); ok {
			names = append(names, actiondocs.GeneralAction)
			_ = general // used below via Get
		}
		sort.Strings(names)

		for _, aName := range names {
			if action != "" && aName != action {
				continue
			}
			doc, ok := reg.Get(sName, aName)
			if !ok {
				continue
			}
			actions[aName] = doc
			resp.Count++
		}
		resp.Actions[sName] = actions
	}

	for _, pe := range reg.ParseErrors() {
		resp.ParseErrors = append(resp.ParseErrors, actionDocsParseError{
			SourceFile: pe.SourceFile,
			Err:        pe.Err,
		})
	}
}

// splitFirst splits a "surface.action" key on the first dot. Returns
// the original key as surface and empty action on malformed input so
// callers see policy entries without silent drops. Mirrors
// observehttp/admin.go's splitSurfaceAction but tolerates the
// no-dot case rather than skipping.
func splitFirst(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
