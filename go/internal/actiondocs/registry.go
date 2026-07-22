package actiondocs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// GeneralAction is the reserved action name for surface-wide cross-cutting
// chunks. It is findable via Get(surface, GeneralAction) but excluded from
// List(surface) so list-by-surface enumerations return only real actions.
const GeneralAction = "_general"

// Param describes one parameter of an action call. The Type field is a
// free-form string at this stage (e.g. "string", "optional_string",
// "json object") — the registry validates structural presence, not
// param-type vocabulary.
//
// JSON tags mirror the TOML tags so admin.action_describe emits the
// same field names on the wire as appear in the source chunk; agents
// reading the corpus disk shape and the API response shape see one
// schema, not two.
type Param struct {
	Name        string `toml:"name"        json:"name"`
	Type        string `toml:"type"        json:"type"`
	Required    bool   `toml:"required"    json:"required"`
	Description string `toml:"description,omitempty" json:"description"`
	Default     string `toml:"default,omitempty" json:"default,omitempty"`
}

// ParamAlias is a parameter-name alias (e.g. `sha` → `commit_sha`).
// Distinct from ValueAlias: ParamAlias renames the KEY; ValueAlias
// renames the VALUE.
type ParamAlias struct {
	From  string `toml:"from"  json:"from"`
	To    string `toml:"to"    json:"to"`
	Notes string `toml:"notes,omitempty" json:"notes,omitempty"`
}

// ValueAlias is a value-form normalization for a specific param (e.g.
// `fix` → `fixed` on the `resolution_kind` param). The Param field scopes
// which param the alias applies to so the same From literal can mean
// different things across params.
type ValueAlias struct {
	Param string `toml:"param" json:"param"`
	From  string `toml:"from"  json:"from"`
	To    string `toml:"to"    json:"to"`
	Notes string `toml:"notes,omitempty" json:"notes,omitempty"`
}

// ErrorCondition documents a caller-controlled error the action can
// return. Runtime/infra failures (DB down, network) are intentionally
// out of scope — only errors the caller controls by shaping their input.
type ErrorCondition struct {
	Condition string `toml:"condition" json:"condition"`
	Message   string `toml:"message"   json:"message"`
}

// Example is a call example. The Call field is a JSON-shaped string
// (NOT live-executable code) showing the canonical params payload.
type Example struct {
	Description string `toml:"description,omitempty" json:"description"`
	Call        string `toml:"call"        json:"call"`
}

// EnvelopeRequirement (bug 1437) describes a field that the dispatcher
// requires on the call envelope — i.e. alongside `action` / `params` /
// `project` on the wire, NOT inside the action's `params` payload. The
// canonical example is `rationale`, enforced by the dispatch policy
// gate for ~30 mutating actions across the work / knowledge / measure
// / admin surfaces.
//
// Why this is separate from Params: per-action `params` describe the
// action's payload schema (what goes inside `params:{...}`).
// EnvelopeRequirement describes wire-level fields that sit at the same
// level as `action` and `params` themselves and are enforced by the
// dispatcher (not the per-action handler). Conflating the two confused
// the bug 1436 trap — agents reading params lists missed rationale
// because rationale isn't a param.
type EnvelopeRequirement struct {
	Field               string   `toml:"field"     json:"field"`
	Required            bool     `toml:"required"  json:"required"`
	Reason              string   `toml:"reason,omitempty"     json:"reason,omitempty"`
	AppliesToActorKinds []string `toml:"applies_to_actor_kinds,omitempty" json:"applies_to_actor_kinds,omitempty"`
}

// ReturnSpec describes the shape of the action's success response. shape
// is the named type (e.g. "BatchResult") that documents the canonical
// envelope; description is one paragraph naming the load-bearing
// fields the agent should expect to find.
//
// Optional on every action — most action docs don't author a [returns]
// block today because the action's return shape is documented in code
// or in nearby docs (CODEMAP, surface README). Authoring [returns] is
// for actions whose return envelope is non-obvious or carries a stable
// contract callers should depend on (e.g. `batch` returns per-op
// outcomes; `chain_state` returns a denormalized projection).
type ReturnSpec struct {
	Shape       string `toml:"shape,omitempty"       json:"shape,omitempty"`
	Description string `toml:"description,omitempty" json:"description,omitempty"`
}

// ActionDoc is one parsed per-action documentation chunk. The required
// fields (Surface, Action, Purpose) are enforced at load time; the
// remaining fields are optional and left zero-valued when the source
// chunk omits them.
type ActionDoc struct {
	Surface              string                `toml:"surface" json:"surface"`
	Action               string                `toml:"action"  json:"action"`
	Purpose              string                `toml:"purpose" json:"purpose"`
	Params               []Param               `toml:"params,omitempty"        json:"params,omitempty"`
	ParamAliases         []ParamAlias          `toml:"param_aliases,omitempty" json:"param_aliases,omitempty"`
	ValueAliases         []ValueAlias          `toml:"value_aliases,omitempty" json:"value_aliases,omitempty"`
	Errors               []ErrorCondition      `toml:"errors,omitempty"        json:"errors,omitempty"`
	Examples             []Example             `toml:"examples,omitempty"      json:"examples,omitempty"`
	Notes                string                `toml:"notes,omitempty"         json:"notes,omitempty"`
	EnvelopeRequirements []EnvelopeRequirement `toml:"envelope_requirements,omitempty" json:"envelope_requirements,omitempty"`
	Returns              *ReturnSpec           `toml:"returns,omitempty" json:"returns,omitempty"`
}

// ParseError records a chunk file that existed on disk but failed to
// load. Surfaced via Registry.ParseErrors() so authoring mistakes show
// up at startup rather than as silent miss results later.
type ParseError struct {
	SourceFile string
	Err        string
}

// Registry holds parsed action-doc chunks indexed by (surface, action).
// Population happens during Load() before the registry is exposed; the
// mutex guards against concurrent Get/List calls from multiple dispatch
// goroutines, even though the chain's commitment is load-once-at-startup
// with no mutation after Load returns.
type Registry struct {
	mu          sync.RWMutex
	entries     map[string]map[string]*ActionDoc // surface → action → doc
	parseErrors []ParseError
}

// New returns an empty registry. Use Load for the standard path.
func New() *Registry {
	return &Registry{entries: make(map[string]map[string]*ActionDoc)}
}

// LoadEmbedded returns a registry populated from the corpus baked into
// the binary at compile time (see embed.go). This is the production
// path: flagless stdio and the HTTP daemon both serve the embedded
// corpus, so admin.action_describe is always available without a
// --action-docs-dir flag resolving a real path at startup.
//
// Parse-failure and validation semantics are identical to Load — the
// two share loadFS. ParseError.SourceFile paths are framed under
// "corpus/" for the embedded set.
func LoadEmbedded() (*Registry, error) {
	sub, err := fs.Sub(corpusFS, "corpus")
	if err != nil {
		return nil, fmt.Errorf("actiondocs: sub embedded corpus: %w", err)
	}
	return loadFS(sub, "corpus")
}

// Load scans dir for <surface>/<action>.toml files and returns a
// registry populated with every successfully-parsed chunk. It is the
// dev/hot-reload override path (--action-docs-dir): production uses
// LoadEmbedded. The top-level _schema.toml and README.md are skipped —
// they describe the corpus, they are not chunks.
//
// A missing dir is NOT an error: Load returns an empty registry so the
// binary can run without the corpus. The returned error is reserved for
// I/O failures on the top-level dir itself (stat / read directory
// failures other than not-exist).
//
// Parse failures are captured into ParseErrors() rather than returned
// as errors — a single broken chunk does not abort the whole load.
// Per-file errors include the file path + reason.
func Load(dir string) (*Registry, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return New(), nil
		}
		return nil, fmt.Errorf("actiondocs: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("actiondocs: %s is not a directory", dir)
	}
	return loadFS(os.DirFS(dir), dir)
}

// loadFS is the shared scan loop backing both Load (os.DirFS over an
// on-disk corpus dir) and LoadEmbedded (an fs.Sub over the embedded
// corpus). labelRoot is prepended to per-file ParseError.SourceFile
// paths so error reporting names where the chunk came from — the on-disk
// dir for Load, "corpus" for the embedded set. fs.FS always uses
// forward-slash paths; SourceFile is rebuilt with filepath.Join for
// host-native display.
func loadFS(fsys fs.FS, labelRoot string) (*Registry, error) {
	r := New()
	surfaces, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("actiondocs: read corpus root %q: %w", labelRoot, err)
	}
	for _, s := range surfaces {
		if !s.IsDir() {
			continue
		}
		surface := s.Name()
		files, err := fs.ReadDir(fsys, surface)
		if err != nil {
			r.parseErrors = append(r.parseErrors, ParseError{
				SourceFile: filepath.Join(labelRoot, surface),
				Err:        err.Error(),
			})
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".toml") {
				continue
			}
			label := filepath.Join(labelRoot, surface, f.Name())
			data, rerr := fs.ReadFile(fsys, surface+"/"+f.Name())
			if rerr != nil {
				r.parseErrors = append(r.parseErrors, ParseError{
					SourceFile: label,
					Err:        fmt.Errorf("read %s: %w", f.Name(), rerr).Error(),
				})
				continue
			}
			doc, perr := decodeDoc(f.Name(), data)
			if perr != nil {
				r.parseErrors = append(r.parseErrors, ParseError{
					SourceFile: label,
					Err:        perr.Error(),
				})
				continue
			}
			wantAction := strings.TrimSuffix(f.Name(), ".toml")
			if doc.Surface != surface {
				r.parseErrors = append(r.parseErrors, ParseError{
					SourceFile: label,
					Err: fmt.Sprintf(
						"surface mismatch: file under %q, chunk declares %q",
						surface, doc.Surface,
					),
				})
				continue
			}
			if doc.Action != wantAction {
				r.parseErrors = append(r.parseErrors, ParseError{
					SourceFile: label,
					Err: fmt.Sprintf(
						"action mismatch: file named %q, chunk declares %q",
						wantAction, doc.Action,
					),
				})
				continue
			}
			if _, ok := r.entries[surface]; !ok {
				r.entries[surface] = make(map[string]*ActionDoc)
			}
			r.entries[surface][doc.Action] = doc
		}
	}
	return r, nil
}

// decodeDoc parses one TOML chunk's bytes and applies required-field
// validation. The name argument (the chunk's base file name) frames the
// error string so the ParseError entry reads cleanly without redundant
// path repetition.
func decodeDoc(name string, data []byte) (*ActionDoc, error) {
	var doc ActionDoc
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if doc.Surface == "" {
		return nil, fmt.Errorf("%s: surface is required", name)
	}
	if doc.Action == "" {
		return nil, fmt.Errorf("%s: action is required", name)
	}
	if doc.Purpose == "" {
		return nil, fmt.Errorf("%s: purpose is required", name)
	}
	return &doc, nil
}

// Get returns the doc for (surface, action). The second return is false
// on a miss (unknown surface OR unknown action under that surface). The
// reserved action name GeneralAction is findable here — callers that
// want only real actions should use List instead.
func (r *Registry) Get(surface, action string) (*ActionDoc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bySurface, ok := r.entries[surface]
	if !ok {
		return nil, false
	}
	doc, ok := bySurface[action]
	return doc, ok
}

// List returns every real action under surface in lexicographic order.
// The reserved GeneralAction is excluded; use Get(surface, GeneralAction)
// to fetch it explicitly.
func (r *Registry) List(surface string) []*ActionDoc {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bySurface, ok := r.entries[surface]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(bySurface))
	for name := range bySurface {
		if name == GeneralAction {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*ActionDoc, 0, len(names))
	for _, name := range names {
		out = append(out, bySurface[name])
	}
	return out
}

// Names returns the sorted list of real action names registered under
// surface (GeneralAction excluded). Symmetrical with List for callers
// that just need names — useful for the miss messaging in T4's getter
// ("action q not registered under work; have: …").
func (r *Registry) Names(surface string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bySurface, ok := r.entries[surface]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(bySurface))
	for name := range bySurface {
		if name == GeneralAction {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Surfaces returns the sorted list of registered surfaces. Useful for
// miss messaging when the caller's surface is unknown.
func (r *Registry) Surfaces() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for s := range r.entries {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Len reports the total number of loaded chunks across all surfaces
// (including any GeneralAction chunks).
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, bySurface := range r.entries {
		n += len(bySurface)
	}
	return n
}

// ParseErrors returns every parse failure seen during Load(). The slice
// is a fresh copy; callers may retain it without holding the registry
// lock.
func (r *Registry) ParseErrors() []ParseError {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]ParseError(nil), r.parseErrors...)
}
