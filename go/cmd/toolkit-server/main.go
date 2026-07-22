package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"toolkit/internal/actiondocs"
	"toolkit/internal/admin"
	"toolkit/internal/arcreview"
	"toolkit/internal/construct"
	"toolkit/internal/db"
	"toolkit/internal/dispatch"
	"toolkit/internal/dispatch/policy"
	"toolkit/internal/ecosystem"
	"toolkit/internal/eventbus"
	"toolkit/internal/events"
	"toolkit/internal/forge/fieldvalue"
	"toolkit/internal/forge/registry"
	"toolkit/internal/grounding"
	"toolkit/internal/inference/llamacpp"
	"toolkit/internal/inference/modelrank"
	"toolkit/internal/inference/router"
	"toolkit/internal/knowledge"
	"toolkit/internal/measure"
	"toolkit/internal/ml"
	"toolkit/internal/obs"
	"toolkit/internal/observehttp"
	"toolkit/internal/projections"
	"toolkit/internal/refresolve"
	"toolkit/internal/rubric"
	"toolkit/internal/sys"
	"toolkit/internal/telemetry"
	"toolkit/internal/work"
)

// gitSHA and builtAtUnix are populated at build time via -ldflags -X
// (see go/Makefile's LDFLAGS). When the binary is built without the
// flags (go run, bare `go build` outside the Makefile), they keep the
// "unversioned" / 0 defaults — same sentinel admin.Deps used before
// build-time injection landed. Surfaced through admin.server_version
// (MCP) and the /version HTTP endpoint so the dashboard can detect a
// daemon that's drifted behind committed source (bug 1415).
var (
	gitSHA      = "unversioned"
	builtAtUnix = "0"
)

// blueprintsDirAuto is the sentinel default for --blueprints-dir that
// triggers binary-relative auto-discovery. A plain "" remains the explicit
// opt-out (skip schema loading) per bug 1266's constraint.
const blueprintsDirAuto = "@auto"

// dispatchPolicyPathAuto is the sentinel default for --dispatch-policy
// that triggers binary-relative auto-discovery, mirroring the
// blueprintsDirAuto pattern. A plain "" disables policy enforcement (the
// dispatcher treats every action as ungated).
const dispatchPolicyPathAuto = "@auto"

// actionDocsDirAuto is the sentinel default for --action-docs-dir. Unlike
// the blueprints/rubrics @auto sentinels (which auto-discover an on-disk
// dir), @auto here means "serve the corpus embedded in the binary"
// (actiondocs.LoadEmbedded) — the corpus moved under the Go module so
// go:embed can bake it in (chain single-source-action-describe T6), which
// is why flagless stdio always serves full docs. An explicit dir path
// overrides with an on-disk load (dev/hot-reload); a plain "" disables the
// corpus entirely (admin.action_describe surfaces a corpus-not-loaded
// envelope) for the rare degraded-mode case.
const actionDocsDirAuto = "@auto"

// rubricsDirAuto is the sentinel default for --rubrics-dir that
// triggers binary-relative auto-discovery, mirroring the
// blueprintsDirAuto pattern. A plain "" disables rubric loading
// entirely (classify_* actions and the resolve_references domain-term
// path degrade to no-classifier mode). Added to close the gap where
// user-scope ~/.claude.json invocations that omitted --rubrics-dir
// silently disabled every rubric-dependent surface — bug
// "rubrics-dir-has-no-binary-relative-auto-discovery".
const rubricsDirAuto = "@auto"

// resolveBlueprintsDir returns the schema dir to load. If the caller passed
// the @auto sentinel (i.e. omitted the flag), looks for the binary-relative
// canonical path. Returns "" — meaning skip loading — when the auto-search
// target doesn't exist or the executable path can't be determined, so the
// existing degraded-mode path runs.
func resolveBlueprintsDir(flagValue string) string {
	if flagValue != blueprintsDirAuto {
		return flagValue
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return autoDiscoverBlueprintsDir(exe)
}

// autoDiscoverBlueprintsDir is split out so tests can drive it with a fake
// exe path without depending on the real os.Executable(). Binary lives in
// `<project>/go/bin/toolkit-server`, schemas in
// `<project>/blueprints/forge-schemas` — two levels up + over.
func autoDiscoverBlueprintsDir(exePath string) string {
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	candidate := filepath.Join(filepath.Dir(exePath), "..", "..", "blueprints", "forge-schemas")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}

// resolveRubricsDir returns the rubrics dir to load. Mirrors
// resolveBlueprintsDir: @auto means binary-relative
// (`<project>/blueprints/rubrics`); explicit path passes through;
// empty string disables rubric loading.
func resolveRubricsDir(flagValue string) string {
	if flagValue != rubricsDirAuto {
		return flagValue
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return autoDiscoverRubricsDir(exe)
}

// autoDiscoverRubricsDir is split out so tests can drive it with a
// fake exe path. Binary lives in `<project>/go/bin/toolkit-server`,
// rubrics in `<project>/blueprints/rubrics` — two levels up + over.
func autoDiscoverRubricsDir(exePath string) string {
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	candidate := filepath.Join(filepath.Dir(exePath), "..", "..", "blueprints", "rubrics")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}

// resolveDispatchPolicyPath returns the dispatch policy TOML path to load.
// Mirrors resolveBlueprintsDir: @auto means binary-relative
// (`<project>/action-manifests/dispatch-policy.toml`); explicit path
// passes through; empty string disables policy enforcement entirely.
func resolveDispatchPolicyPath(flagValue string) string {
	if flagValue != dispatchPolicyPathAuto {
		return flagValue
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	candidate := filepath.Join(filepath.Dir(exe), "..", "..", "action-manifests", "dispatch-policy.toml")
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		return candidate
	}
	return ""
}

func main() {
	// Subcommand dispatch — sniff os.Args[1] before the top-level
	// flag.Parse so rebuild-projections owns its own flag set. Server
	// mode is the default when no subcommand is supplied (preserves
	// the pre-T4 invocation surface).
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "rebuild-projections":
			os.Exit(runRebuildProjections(os.Args[2:]))
		case "backfill-a1-rubric-names":
			os.Exit(runBackfillA1RubricNames(os.Args[2:]))
		case "exercise-chain-assessment-dispatch":
			os.Exit(runExerciseChainAssessmentDispatch(os.Args[2:]))
		case "smoke-classify-rubric":
			os.Exit(runSmokeClassifyRubric(os.Args[2:]))
		case "regression-knowledge-search":
			os.Exit(runRegressionKnowledgeSearch(os.Args[2:]))
		case "regression-runner":
			os.Exit(runRegressionRunner(os.Args[2:]))
		case "qwen-vault-smoke-backfill":
			os.Exit(runQwenVaultSmokeBackfill(os.Args[2:]))
		case "--version", "-version", "version":
			// Short-circuit before any auto-discovery or --db
			// requirement so the build SHA is queryable on a fresh
			// or partially-set-up checkout. The same ldflags-
			// injected values power admin.server_version's response.
			builtAt := "unknown"
			if n, err := strconv.ParseInt(builtAtUnix, 10, 64); err == nil && n > 0 {
				builtAt = time.Unix(n, 0).UTC().Format(time.RFC3339)
			}
			fmt.Printf("%s built %s\n", gitSHA, builtAt)
			return
		}
	}

	var dbPath string
	var defaultProject string
	var rubricsDir string
	var llamaURL string
	var blueprintsDir string
	var dispatchPolicyPath string
	var actionDocsDir string
	var httpPort int
	var httpOnly bool

	flag.StringVar(&dbPath, "db", "", "path to toolkit SQLite database")
	flag.StringVar(&defaultProject, "default-project", "", "default project slug for unscoped queries")
	flag.StringVar(&rubricsDir, "rubrics-dir", rubricsDirAuto, "path to rubric TOML definitions directory (default: auto-discover relative to binary; pass \"\" to skip loading)")
	flag.StringVar(&llamaURL, "llama-url", llamacpp.BaseURLFromEnv(), "llama.cpp server base URL (env precedence: TOOLKIT_LOCAL_URL, then LLAMA_CPP_BASE_URL, then http://localhost:8081)")
	flag.StringVar(&blueprintsDir, "blueprints-dir", blueprintsDirAuto, "path to forge schema TOML directory (default: auto-discover relative to binary; pass \"\" to skip loading)")
	flag.StringVar(&dispatchPolicyPath, "dispatch-policy", dispatchPolicyPathAuto, "path to action-manifests/dispatch-policy.toml (default: auto-discover relative to binary; pass \"\" to disable rationale enforcement)")
	flag.StringVar(&actionDocsDir, "action-docs-dir", actionDocsDirAuto, "per-action documentation corpus (default: serve the corpus embedded in the binary; pass a dir to override with an on-disk load for dev/hot-reload; pass \"\" to disable the corpus)")
	flag.IntVar(&httpPort, "http-port", 0, "observe HTTP port (e.g. 3000); 0 disables the HTTP surface")
	flag.BoolVar(&httpOnly, "http-only", false, "skip stdio MCP transport; serve only the HTTP surface")
	flag.Parse()

	if dbPath == "" {
		fmt.Fprintln(os.Stderr, "error: --db is required")
		os.Exit(1)
	}

	obs.InitFromEnv()
	obs.L().Info("toolkit-server starting",
		slog.String("db", dbPath),
		slog.String("project", defaultProject),
		slog.String("rubrics_dir", rubricsDir),
	)

	pool, err := db.Open(dbPath)
	if err != nil {
		obs.Fatalf("open db: %v", err)
	}
	defer pool.Close()

	// Install the persistent span sink BEFORE the first dispatch can fire,
	// in EVERY process (stdio MCP or HTTP daemon). The dashboard's
	// /events/spans stream tails the shared span_events table — see bug
	// live-spans-empty-spanbus-per-process-cross-process-gap for why the
	// previous in-process bus was invisible across processes.
	obs.SetSpanSink(obs.NewDBSpanSink(pool.DB()))

	// Wire the projections fold hook BEFORE any handler dispatch is
	// possible. Closure adapts events.RawEvent → projections.RawEvent
	// (field-by-field identical; the seam is the dependency reversal,
	// not a shape transformation). Hook fires inside every Emit's tx;
	// projection refresh failure rolls the originating mutation back.
	events.SetFoldHook(func(ctx context.Context, tx *sql.Tx, evt events.RawEvent) error {
		return projections.FoldAll(ctx, tx, projections.RawEvent{
			EventID:         evt.EventID,
			Ts:              evt.Ts,
			ActorKind:       evt.ActorKind,
			ActorID:         evt.ActorID,
			Type:            evt.Type,
			EntityKind:      evt.EntityKind,
			EntitySlug:      evt.EntitySlug,
			EntityProjectID: evt.EntityProjectID,
			Payload:         evt.Payload,
			Rationale:       evt.Rationale,
			CausedByEventID: evt.CausedByEventID,
			RelatedEntities: evt.RelatedEntities,
			SpanID:          evt.SpanID,
			SchemaVersion:   evt.SchemaVersion,
		})
	})
	registerProjectionsFoldHook()

	// Read-side fold trigger for the query-telemetry-substrate
	// projections (TT3). Telemetry emits (EmitInteraction /
	// EmitResolution) run the hook inside their write tx so fold
	// failure rolls the emit back — same discipline as the write-side
	// events fold hook above.
	telemetry.SetFoldHook(projections.FoldAllReadSide)

	// NOTE: the work_tool_calls dispatch-telemetry writer was removed here
	// when work_tool_calls was retired (chain per-tool-per-model-
	// observability T12 — it was a dead, per-ACTION sink that the misleading
	// migration-075 comment falsely framed as this chain's precursor). The
	// generic dispatch.CallObserver seam is intentionally retained (it takes
	// no DB dependency and is zero-cost while unset); a future dispatch-
	// telemetry chain can re-wire it onto the read-side substrate.

	// arc-close-filing-review-substrate-listener-wiring T4: chain a
	// substrate-trigger detector in front of the projections fold hook
	// and dispatch real reviews via SubstrateReviewObserver. The
	// observer resolves the project's most-recently-active session
	// (written by hooks/arc-close-filing-review-hook.sh, T3) and fires
	// HandleReviewArcForFiling on a goroutine so the fold tx exits
	// cleanly. Successful fires enqueue a pending_decisions row that
	// the next Stop hook claims and surfaces (T5).
	//
	// Router is wired below in arcReviewDeps; we have to install the
	// observer after Router is available, so this call moved to the
	// post-router site (search for "arcreview.NewSubstrateReviewObserver").

	// Inference router — Qwen (required) + Anthropic (optional, from env).
	inferRouter := router.NewWithClients(
		llamacpp.New(llamaURL),
		nil, // Anthropic client wired via New() in production when key is present
		"qwen2.5-32b",
	)
	// Per-call inference telemetry. Persistence failure logs-and-drops so
	// a telemetry-write outage never blocks the inference response itself.
	//
	// T12 cutover (chain per-tool-per-model-observability): the
	// /inference endpoints now read inference_invocations, so the
	// transitional qwen_invocations dual-write is removed — this records
	// only the new read-side substrate table (model-agnostic,
	// +success/error_class, +remote coverage). The empty qwen_invocations
	// table soaks until its DROP in Chain 5. Fail-open: a telemetry-write
	// outage never blocks the inference response.
	inferRouter.SetInvocationRecorder(func(ctx context.Context, rec router.InvocationRecord) {
		if _, err := db.RecordInferenceInvocation(ctx, pool, db.InferenceInvocation{
			TaskID:       rec.TaskID,
			ModelName:    rec.ModelName,
			LatencyMS:    rec.LatencyMS,
			InputTokens:  rec.InputTokens,
			OutputTokens: rec.OutputTokens,
			Success:      rec.Success,
			ErrorClass:   rec.ErrorClass,
		}); err != nil {
			obs.Logger(ctx).Warn("inference_invocations telemetry write failed",
				slog.String("err", err.Error()))
		}
		// Refresh the per-(tool,model) performance projection. Read-side
		// re-snapshot (idempotent), kept separate + fail-open from the row
		// write: a skipped fold self-heals on the next call's re-snapshot,
		// and a fold outage must never block inference. Targeted to this one
		// projection so unrelated read-side projections aren't rebuilt on
		// every model call.
		if err := pool.WithWrite(ctx, func(tx *sql.Tx) error {
			_, err := projections.RebuildAll(ctx, tx, []string{"inference_tool_model_performance"})
			return err
		}); err != nil {
			obs.Logger(ctx).Warn("inference_tool_model_performance projection refresh failed",
				slog.String("err", err.Error()))
		}
	})

	// Data-driven model selection (chain data-driven-model-routing). The
	// router consults this selector per call; it reads the per-(tool,model)
	// performance projection the recorder above refreshes, through a 60s
	// cache, and falls back to the static default ("qwen2.5-32b") at cold
	// start / on any read error. Best-effort: a telemetry-read outage degrades
	// to the static default, never blocks or misroutes the inference call.
	inferRouter.SetModelSelector(modelrank.NewRanker(pool, "qwen2.5-32b").Select)

	// Rubric registry — optional; classify actions degrade gracefully when absent.
	rubricsDir = resolveRubricsDir(rubricsDir)
	var rubricReg *rubric.Registry
	if rubricsDir != "" {
		rubricReg, err = rubric.NewRegistry(rubricsDir)
		if err != nil {
			obs.Fatalf("load rubrics: %v", err)
		}
		obs.L().Info("loaded rubrics",
			slog.Int("count", countRubrics(rubricReg)),
			slog.String("dir", rubricsDir),
		)
	}

	deps := measure.ClassifyDeps{
		Pool:    pool,
		Router:  inferRouter,
		Rubrics: rubricReg,
		Project: defaultProject,
	}

	// Event bus is allocated here (before the measure table is built) so the
	// study_run_record handler can publish an artifact_created SSE event on
	// commit — the dashboard's live study-run refresh. Allocated only when
	// --http-port is set (there's a /events sink to stream to); nil otherwise,
	// and the handler skips the publish. The spanTail sink is still created in
	// the httpPort block below alongside the router wiring.
	var bus *eventbus.Bus
	if httpPort != 0 {
		bus = eventbus.New(0)
	}

	measureTable := measure.BuildTable(deps, measure.BenchmarkDeps{Pool: pool}, measure.StudyRunDeps{Pool: pool, Bus: bus}, measure.GateRunDeps{Pool: pool})

	// Dispatch policy registry — optional; degraded mode skips rationale
	// enforcement entirely. Loaded ahead of the dispatch tables so the
	// MCP tool-registration closures can capture it by value.
	dispatchPolicyPath = resolveDispatchPolicyPath(dispatchPolicyPath)
	policyReg, err := policy.Load(dispatchPolicyPath)
	if err != nil {
		obs.Fatalf("load dispatch policy: %v", err)
	}
	if dispatchPolicyPath == "" {
		obs.L().Info("dispatch policy disabled (no path)")
	} else {
		obs.L().Info("loaded dispatch policy",
			slog.Int("entries", policyReg.Len()),
			slog.String("path", dispatchPolicyPath),
		)
	}

	// Forge schema registry — optional; forge action degrades when absent.
	// Resolve @auto sentinel to the binary-relative path here so the load
	// site stays a simple non-empty check.
	blueprintsDir = resolveBlueprintsDir(blueprintsDir)
	var schemaReg *registry.Registry
	if blueprintsDir != "" {
		schemaReg, err = registry.Load(blueprintsDir)
		if err != nil {
			obs.Fatalf("load blueprints: %v", err)
		}
		obs.L().Info("loaded schemas",
			slog.Int("count", schemaReg.Len()),
			slog.String("dir", blueprintsDir),
		)
		if errs := schemaReg.ParseErrors(); len(errs) > 0 {
			for _, e := range errs {
				obs.L().Warn("schema parse error",
					slog.String("source_dir", e.SourceDir),
					slog.String("source_file", e.SourceFile),
					slog.String("err", e.Err),
				)
			}
		}
	}

	// Per-action documentation corpus. Default (@auto) serves the corpus
	// embedded in the binary (always present → flagless stdio has full
	// docs). An explicit dir overrides with an on-disk load for
	// dev/hot-reload. A plain "" disables the corpus entirely. A bad chunk
	// is collected as a ParseError, not a fatal: the binary still serves
	// against the rest of the corpus.
	var actionDocsReg *actiondocs.Registry
	var actionDocsOverrideDir string // non-empty only when --action-docs-dir overrides to an on-disk dir
	switch actionDocsDir {
	case actionDocsDirAuto:
		actionDocsReg, err = actiondocs.LoadEmbedded()
		if err != nil {
			obs.Fatalf("load embedded action-docs: %v", err)
		}
		obs.L().Info("loaded action-docs corpus",
			slog.Int("count", actionDocsReg.Len()),
			slog.String("source", "embedded"),
		)
	case "":
		// Explicit opt-out: admin.action_describe + the dashboard browser
		// degrade to a corpus-not-loaded envelope. Rare degraded-mode case.
		obs.L().Info("action-docs corpus disabled (--action-docs-dir=\"\")")
	default:
		actionDocsOverrideDir = actionDocsDir
		actionDocsReg, err = actiondocs.Load(actionDocsDir)
		if err != nil {
			obs.Fatalf("load action-docs: %v", err)
		}
		obs.L().Info("loaded action-docs corpus",
			slog.Int("count", actionDocsReg.Len()),
			slog.String("source", actionDocsDir),
		)
	}
	if actionDocsReg != nil {
		if errs := actionDocsReg.ParseErrors(); len(errs) > 0 {
			for _, e := range errs {
				obs.L().Warn("action-docs parse error",
					slog.String("source_file", e.SourceFile),
					slog.String("err", e.Err),
				)
			}
		}
	}
	// actionDocsReg threads into admin.Deps below; the dispatch table
	// always registers action_describe but the handler degrades to a
	// corpus-not-loaded envelope when actionDocsReg is nil.

	// Event bus — optional; the work surface emits create events via the
	// forge OnCreate callback when the bus is configured. The bus is
	// allocated whenever --http-port is set so /events SSE has a sink to
	// stream from; the legacy --event-bus-addr standalone listener has
	// been retired in favour of mounting /events on the observe-http
	// router.
	// bus was allocated earlier (before the measure table) so study_run_record
	// could capture it; spanTail is created here alongside the router wiring.
	var spanTail *obs.SpanTail
	if httpPort != 0 {
		spanTail = obs.NewSpanTail(pool.DB())
	}

	// Parse the ldflags-injected builtAtUnix string into the int64 the
	// admin/observehttp consumers expect. Bad-format input from a
	// hand-overridden -ldflags falls back to 0 — same as the no-flags
	// default. Per bug 1415 the value is informational (banner text);
	// no enforcement hinges on it being non-zero. Hoisted above the
	// httpPort block so both the HTTP AppState and the adminTable
	// below can read the parsed value.
	parsedBuiltAt, _ := strconv.ParseInt(builtAtUnix, 10, 64)

	if httpPort != 0 {
		addr := fmt.Sprintf(":%d", httpPort)
		// repoRoot for the daemon side may not have been computed yet at
		// this point in main(); derive a local copy from blueprintsDir
		// so AppState carries it for /admin/stdio-drift-state's git
		// rev-parse HEAD probe. Empty when blueprintsDir is unset
		// (degraded boot); the snapshot helper falls back to the cwd.
		var observeRepoRoot string
		if blueprintsDir != "" {
			observeRepoRoot = filepath.Dir(filepath.Dir(blueprintsDir))
		}
		router := observehttp.BuildRouter(observehttp.AppState{
			Pool:               pool,
			Bus:                bus,
			SpanTail:           spanTail,
			DispatchPolicyPath: dispatchPolicyPath,
			ActionDocs:         actionDocsReg,
			ActionDocsDir:      actionDocsOverrideDir,
			GitSHA:             gitSHA,
			BuiltAtUnix:        parsedBuiltAt,
			PackageVer:         "v0.1.0",
			RepoRoot:           observeRepoRoot,
		})
		srv := &http.Server{
			Addr:              addr,
			Handler:           router,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			obs.L().Info("observe HTTP listening", slog.String("addr", addr))
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				obs.Fatalf("observe HTTP exited: %v", err)
			}
		}()

		// Retention janitor: prune span_events older than 24 h every 5
		// minutes. HTTP daemon only — stdio MCPs don't run this to avoid
		// fighting for the write lock.
		go spanEventsJanitor(spanTail)
	}

	// forge archived in chain 311 T7 Stage 6 P2-C.2: the per-shape Strategy
	// registry + its boot-integrity gate retired with it (construct's per-schema
	// dispatch is name-keyed, not registry-driven). The schema registry itself is
	// still validated at load time (registry.Register).

	workTable := work.BuildTable(work.TableDeps{
		Pool:    pool,
		Schemas: schemaReg,
		Bus:     bus,
		// Route batch's forge create/edit ops through the construct umbrella on
		// the outer batch tx. Wired here (work can't import construct). REQUIRED
		// now — the forge in-tx fallback retired with the archive.
		ForgeCreateInTx: batchForgeCreateInTx(pool, schemaReg, bus),
		ForgeEditInTx:   batchForgeEditInTx(pool, schemaReg),
	})

	// Chain 311 T7 Stage 6 P2-C.2 (forge archive): the forge / forge_edit /
	// forge_delete / forge_schema / forge_schemas actions are now wired HERE on
	// the construct umbrella (forge is gone). They register in main, not
	// work.BuildTable, because the handlers need construct.Deps and work can't
	// import construct (the construct→work cycle).
	//
	// construct.Deps for the create/update/delete orchestration (Pool + Schemas).
	forgeActionConstructDeps := construct.Deps{Pool: pool, Schemas: schemaReg}
	// Shared per-process burst tracker (bug 887): this is the sole create path,
	// so the per-session burst count stays correct.
	burstTracker := construct.NewForgeBurstTracker()
	// CREATE finalize deps. Two variants:
	//   - sse: SSE publish only. Used by every covered create EXCEPT vault-note —
	//     construct.Create already synced their knowledge_pointer, so re-running
	//     the index upsert would double-write AND mis-report action="updated" on a
	//     fresh create.
	//   - full: SSE publish + the index-upsert notifier. Used by vault-note create
	//     (construct.Create writes only the file there) so the pointer upsert + the
	//     same-slug-reforge "updated" verb + scope-change orphan cleanup happen —
	//     exactly as forge's AfterCreate notifier did. Shares the burst tracker
	//     pointer so the count stays consistent across both variants.
	createFinalizeSSEDeps := construct.Deps{Pool: pool, Schemas: schemaReg, OnCreate: makeOnCreateNotifier(bus), BurstTracker: burstTracker}
	createFinalizeFullDeps := construct.Deps{Pool: pool, Schemas: schemaReg, OnCreate: chainNotifiers(makeOnCreateNotifier(bus), construct.IndexUpsertNotifier(pool)), BurstTracker: burstTracker}
	// EDIT finalize deps: SSE-less; the OnEdit index notifier reads the post-edit
	// canonical state back and refreshes the pointer (idempotent re-run after the
	// covered edits' own IndexSyncFromProjection; the file read-back for vault-note).
	editFinalizeDeps := construct.Deps{Pool: pool, Schemas: schemaReg, OnEdit: construct.IndexUpsertOnEditNotifier(pool, schemaReg)}

	workTable["forge"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (construct.ForgeCreateResult, error) {
		return construct.HandleForgeCreate(ctx, forgeActionConstructDeps, createFinalizeSSEDeps, createFinalizeFullDeps, project, params)
	})
	workTable["forge_edit"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (construct.ForgeEditResult, error) {
		return construct.HandleForgeEdit(ctx, forgeActionConstructDeps, editFinalizeDeps, project, params)
	})
	workTable["forge_delete"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (construct.ForgeDeleteResult, error) {
		return construct.HandleForgeDelete(ctx, forgeActionConstructDeps, project, params)
	})
	// forge_schema / forge_schemas: read-only introspection over the schema
	// registry, re-homed from forge/introspect.go into construct (P2-C.2).
	workTable["forge_schemas"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (construct.ForgeSchemasResult, error) {
		return construct.HandleForgeSchemas(ctx, forgeActionConstructDeps, project, params)
	})
	workTable["forge_schema"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (construct.ForgeSchemaResult, error) {
		return construct.HandleForgeSchema(ctx, forgeActionConstructDeps, project, params)
	})

	// Chain 311 T7 Stage 5 (P1-D): the `record` action's forge-shaped sugar mode —
	// {schema_name, slug, fields[, op]} (op ∈ create|update|delete, default create) —
	// routes through the construct umbrella via the same handlers the forge /
	// forge_edit / forge_delete actions use. The raw events[] mode (HandleRecord)
	// stays for lifecycle events + multi-event sequences. This is the agent-facing
	// write surface that survives forge's archival.
	recordSugarDeps := work.TableDeps{Pool: pool, Bus: bus}
	workTable["record"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (json.RawMessage, error) {
		return handleRecordOrSugar(ctx, recordSugarDeps, forgeActionConstructDeps,
			createFinalizeSSEDeps, createFinalizeFullDeps, editFinalizeDeps, project, params)
	})

	// arc-close-filing-review T4 Stage 2: register review_arc_for_filing
	// on the work table. Wired externally because the handler needs
	// Router (currently not part of work.TableDeps) and the action is
	// the only work-surface action that does inference today. Routing
	// it through work is per docs/ARC_CLOSE_FILING_REVIEW.md — the
	// review's typed output dispatches to forge_bug / forge_vault_note
	// (both on the work surface), so co-locating the call is the
	// least-surprising shape.
	// The arcreview unreviewed-fallback sweep (memory + vault-note) routes
	// through the SAME construct create path the forge action uses — so a
	// fallback-forged memory/vault-note lands its knowledge_pointer (the
	// unreviewed tag stays queryable) exactly like an agent-issued create.
	// Injected as a closure so arcreview stays free of a construct import and
	// the substrate-doesn't-own-the-write-surface boundary holds as a DI seam.
	// (Chain 311 T7 Stage 6 P2-C.2: vault-note re-homed into construct, so it no
	// longer needs forge here either.)
	arcReviewDeps := arcreview.Deps{
		Pool:   pool,
		Router: inferRouter,
		ForgeFn: func(ctx context.Context, project string, params json.RawMessage) error {
			return arcFallbackForge(ctx, forgeActionConstructDeps, createFinalizeSSEDeps, createFinalizeFullDeps, project, params)
		},
	}
	workTable["review_arc_for_filing"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.ReviewArcForFilingResult, error) {
		return arcreview.HandleReviewArcForFiling(ctx, arcReviewDeps, project, params)
	})
	// arc-close-filing-review-substrate-listener-wiring T5: register
	// pending_decisions_claim on the work table. The Stop hook calls
	// this after every session_registry UPSERT to drain whatever the
	// substrate-side observer enqueued since the last Stop. Co-located
	// with review_arc_for_filing because both share arcReviewDeps.
	workTable["pending_decisions_claim"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.PendingDecisionsClaimResult, error) {
		return arcreview.HandlePendingDecisionsClaim(ctx, arcReviewDeps, project, params)
	})
	// register_session UPSERTs session_registry over the writer mutex so the Stop
	// hook stops opening the canonical DB file directly (the post-cutover single-
	// writer / cross-mount-namespace WAL hazard; bug wired-stop-hooks-open-db-
	// directly-and-target-stale-mcp-servers-path-and-3000). Co-located with the
	// other arcreview actions — shares arcReviewDeps for the Pool.
	workTable["register_session"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.RegisterSessionResult, error) {
		return arcreview.HandleRegisterSession(ctx, arcReviewDeps, project, params)
	})
	// ingest_grounding receives the grounding-events-processor binary's host-parsed
	// transcript output (events+entries) and runs the emit + read-side projection
	// fold via the container's single writer — so the grounding Stop hook no longer
	// opens the canonical DB file directly (bug grounding-events-processing-disabled-
	// pending-container-side-http-ingestion). The fold hook is already wired above.
	workTable["ingest_grounding"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (grounding.Result, error) {
		return grounding.HandleIngest(ctx, pool, project, params)
	})
	// arc-close-filing-review-substrate-listener-wiring T6: emit_commit_landed
	// is invoked by the post-commit advisor (via the
	// go/cmd/commit-landed-emit binary) so the CommitLanded event lands
	// INSIDE the daemon's process — only then does the chained fold hook
	// (SubstrateReviewObserver) fire and queue decisions for the next
	// Stop hook to drain. A standalone one-shot binary opening its own
	// pool would emit the event but bypass the daemon's fold hook.
	workTable["emit_commit_landed"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.EmitCommitLandedResult, error) {
		return arcreview.HandleEmitCommitLanded(ctx, arcReviewDeps, project, params)
	})
	// arc-close-filing-review-substrate-listener-wiring T8: read-side
	// audit surface. Joins ArcCloseFilingReviewed corpus events with
	// pending_decisions dispatch state and heuristic user-correction
	// signals from subsequent events. Feeds T9 (threshold + prompt
	// tuning) and T10 (ML follow-on corpus exports).
	workTable["arc_review_audit"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.ArcReviewAuditResult, error) {
		return arcreview.HandleArcReviewAudit(ctx, arcReviewDeps, project, params)
	})
	// arc-close-decision-authoring-split T5: explicit unreviewed-fallback
	// trigger. A session-end / session-start reaper hook (or an explicit
	// agent skip) calls this to forge the Qwen draft for any staged decision
	// the seat never authored, flagged unreviewed. The in-session trigger
	// (reap-on-next-fire) lives inside review_arc_for_filing; this is the
	// out-of-band capture point for true disengagement.
	workTable["sweep_unauthored_staged"] = dispatch.Adapt(func(ctx context.Context, project string, params json.RawMessage) (arcreview.SweepResult, error) {
		return arcreview.HandleSweepUnauthoredStaged(ctx, arcReviewDeps, project, params)
	})

	// arc-close-filing-review-substrate-listener-wiring T4: install the
	// substrate-trigger observer now that Router (via arcReviewDeps) is
	// available. The observer chains in front of the projections fold
	// hook installed earlier; on every BugResolved / TaskCompleted /
	// ChainClosed (and future CommitLanded / RoadmapUpdated) emit, it
	// resolves the project's session and fires a review on a goroutine.
	//
	// Chain quiet-and-instrument-operator-surface T2: install ONLY in the
	// long-lived HTTP daemon (--http-only), NOT in stdio MCP sessions. The
	// fold hook fires in-process on every emit, so registering it in every
	// toolkit-server process made stdio sessions race the daemon on the
	// per-session debounce and — the 0-live bug — let a stale stdio session
	// emit a capture-less ArcCloseFilingReviewed. Daemon-only is single-
	// instance and matches the original design intent (see the "INSIDE the
	// daemon's process" note on emit_commit_landed above). The hook-driven
	// path (Stop/commit hooks → POST /mcp/work) is unaffected — it already
	// targets the daemon.
	if httpOnly {
		arcreview.InstallListenerFoldHook(arcreview.NewSubstrateReviewObserver(arcReviewDeps))
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "toolkit-server",
		Version: "v0.1.0",
	}, nil)

	adminDeps := admin.Deps{
		Pool:        pool,
		Schemas:     schemaReg,
		ActionDocs:  actionDocsReg,
		StartedAt:   time.Now(),
		GitSHA:      gitSHA,
		BuiltAtUnix: parsedBuiltAt,
		PackageVer:  "v0.1.0",
	}
	adminTable := admin.BuildTable(adminDeps)
	// Fire-and-forget orphan-pointer sweep at bootstrap. Logs the
	// summary at info level on success and warn on failure; never
	// blocks startup. The on-demand admin.vault_integrity_sweep
	// action is the same code path. Chain
	// forge-vault-note-schema-rework T5.
	adminDeps.RunStartupVaultIntegritySweep(context.Background())
	// Legacy health_ping shim — keeps the pre-T59 caller working until
	// callers migrate to admin.health. Drop after the T68 .mcp.json
	// sweep confirms no remaining callers.
	adminTable["health_ping"] = dispatch.Adapt(func(_ context.Context, _ string, _ json.RawMessage) (healthPingResult, error) {
		return healthPingResult{OK: true, Server: "toolkit-server-go"}, nil
	})

	metaSchema := dispatch.MetaToolInputSchema()

	// Load project paths once at startup for CWD-derived default-project
	// resolution. New projects registered via admin.project_register after
	// startup aren't seen until restart — acceptable for now since project
	// churn is rare; a refresh hook is the follow-up.
	projectPaths, err := loadProjectPaths(pool)
	if err != nil {
		obs.L().Warn("project-path snapshot failed (CWD resolution disabled)",
			slog.String("err", err.Error()))
	}
	resolver := dispatch.NewCwdProjectResolver(projectPaths, defaultProject)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "work",
		Description: actiondocs.WorkDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, workTable, args, dispatch.Options{Policy: policyReg, Surface: "work"})
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "measure",
		Description: actiondocs.MeasureDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, measureTable, args, dispatch.Options{Policy: policyReg, Surface: "measure"})
	})

	knowledgeDeps := knowledge.Deps{Pool: pool, Router: inferRouter}
	knowledgeTable := knowledge.BuildTable(knowledgeDeps)

	// Reference-resolution substrate: wire the resolve_references
	// action onto the knowledge meta-tool. The classifier is optional
	// (degrades to rule-based-only detection when nil); the registry
	// is constructed once at startup with full deps.
	var refresolveClassifier refresolve.DomainTermClassifier
	if rubricReg != nil {
		if cls, clsErr := refresolve.NewDomainTermRubricClassifier(rubricReg, inferRouter); clsErr == nil {
			refresolveClassifier = cls
		} else {
			obs.L().Info("reference-resolution: domain-term classifier unavailable, running rule-based only",
				slog.String("reason", clsErr.Error()))
		}
	}
	repoRoot := ""
	if blueprintsDir != "" {
		repoRoot = filepath.Dir(filepath.Dir(blueprintsDir))
	}
	// reference-resolution-migration T10: derive the per-project
	// auto-memory directory from $HOME and the slugified cwd. The
	// convention is ~/.claude/projects/<cwd-slug>/memory/ where the
	// slug replaces filesystem separators with dashes (Claude Code's
	// own convention for the auto-memory dir layout).
	refresolveMemoryDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		cwd, _ := os.Getwd()
		if cwd != "" {
			slug := strings.ReplaceAll(cwd, "/", "-")
			refresolveMemoryDir = filepath.Join(home, ".claude", "projects", slug, "memory")
		}
	}
	refresolveRegistry := refresolve.BuildProductionRegistry(refresolve.ProductionDeps{
		Pool:          pool,
		Project:       defaultProject,
		KnowledgeDeps: knowledgeDeps,
		RepoRoot:      repoRoot,
		MemoryDir:     refresolveMemoryDir,
	})
	refresolveCache := refresolve.NewParseContextCache()
	refresolveWorkStateCache := refresolve.NewWorkStateCache()
	// Chain parse-context-lean-orienting T1 + T6: install the
	// cache-invalidation fold hook so chain/task/bug emits drop
	// stale entries from BOTH the token-keyed parse_context cache
	// (T1) AND the work-state surface cache (T6). Chains in front of
	// the projections hook via CurrentFoldHook capture.
	refresolve.InstallCacheInvalidationFoldHook(refresolveCache, refresolveWorkStateCache)
	// Chain parse-context-lean-orienting T9: per-session drift-fire
	// tracker for the stdio-drift discipline_skill surface. Shared
	// across handler calls so the bootstrap-fire / suppression-counter
	// state survives the request goroutines.
	refresolveDriftTracker := refresolve.NewDriftFireTracker()
	// Chain 602: long-lived BodyCache for skill-body inlining. Mtime-keyed
	// so the same body reads once per session, not once per envelope.
	// Inlining is ON by default since 2026-05-21 (chain 602 T6 follow-up);
	// TOOLKIT_PARSE_CONTEXT_INLINE_BODIES=0 is the kill-switch.
	refresolveBodyCache := refresolve.NewBodyCache()
	refresolveDisciplineFireTracker := refresolve.NewDisciplineFireTracker()
	refresolveDeps := refresolve.HandlerDeps{
		Pool:                  pool,
		Project:               defaultProject,
		KnowledgeDeps:         knowledgeDeps,
		RepoRoot:              repoRoot,
		Classifier:            refresolveClassifier,
		Registry:              refresolveRegistry,
		Cache:                 refresolveCache,
		MemoryDir:             refresolveMemoryDir,
		BodyCache:             refresolveBodyCache,
		GitSHA:                gitSHA,
		DriftFireTracker:      refresolveDriftTracker,
		WorkStateCache:        refresolveWorkStateCache,
		DisciplineFireTracker: refresolveDisciplineFireTracker,
		KiwixFallbackSearch: refresolve.NewKiwixFallbackSearcherFromKnowledge(
			knowledgeDeps, defaultProject,
		),
	}
	knowledgeTable["resolve_references"] = refresolve.BuildResolveReferencesHandler(refresolveDeps)
	// parse_context is the canonical name (reference-resolution-migration
	// T5); resolve_references stays as a soft alias. Both dispatch into
	// the same handler core today — cache + new resolvers
	// (skill_trigger, memory_entry, vault_candidate, kiwix_bridge,
	// discipline_skill) land in follow-on phases.
	knowledgeTable["parse_context"] = refresolve.BuildParseContextHandler(refresolveDeps)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "knowledge",
		Description: actiondocs.KnowledgeDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, knowledgeTable, args, dispatch.Options{Policy: policyReg, Surface: "knowledge"})
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "admin",
		Description: actiondocs.AdminDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, adminTable, args, dispatch.Options{Policy: policyReg, Surface: "admin"})
	})

	// ML inference surface (ml-capability-substrate T5). The registry
	// resolves trained_model rows; convenience actions register on top
	// as per-task chains promote their models.
	mlRegistry := ml.NewRegistry(pool)
	mlTable := ml.BuildTable(ml.TableDeps{Pool: pool, Registry: mlRegistry})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ml",
		Description: actiondocs.MLDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, mlTable, args, dispatch.Options{Policy: policyReg, Surface: "ml"})
	})

	// The owned FILESYSTEM surface (fs) is RETIRED from the toolkit
	// (corpos-substrate-topology T6, decompose-not-delete). fs is a harness-loop
	// family, not a shared-ledger surface: corpos owns it natively
	// (corpos/internal/fsorgan) and Claude Code uses its own Read/Write/Edit;
	// nothing consumes the toolkit fs surface anymore, and a distroless image
	// shouldn't carry the workspace-fs hop (the run-5 fs-namespace breakage). The
	// internal/fs package + its action-doc corpus are RETAINED as the parity
	// oracle, just no longer wired as a live surface. See docs/TOPOLOGY.md.

	// Owned system surface (chain owned-exec-shell-surface). Read-only
	// introspection (ps/ports/units/containers) is ungated and stays here —
	// corpos's native sys organ delegates introspection to it. The gated `exec`
	// action is RETIRED from this surface (T6): exec is host-loop work corpos now
	// owns natively (corpos/internal/sysorgan), and the distroless image has no
	// /bin/sh, so toolkit exec was vestigial.
	sysTable := sys.BuildTable()
	mcp.AddTool(server, &mcp.Tool{
		Name:        "sys",
		Description: actiondocs.SysDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, sysTable, args, dispatch.Options{Policy: policyReg, Surface: "sys"})
	})

	// Local-ecosystem surface (chain 435 local-ecosystem-service-and-extraction-pattern).
	// Deterministic, tenant-agnostic map of the agent-host's world (hosts /
	// services / access methods); answers "do I have access to X" without a RAG
	// round-trip. Reuses the shared `hosts` table; direct-write, ships empty.
	ecosystemTable := ecosystem.BuildTable(ecosystem.Deps{Pool: pool})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ecosystem",
		Description: actiondocs.EcosystemDescription,
		InputSchema: metaSchema,
	}, func(ctx context.Context, req *mcp.CallToolRequest, args dispatch.Args) (*mcp.CallToolResult, any, error) {
		ctx = stampMCPActor(ctx, req)
		ctx = stampMCPSessionID(ctx, req)
		return dispatch.DispatchWithOptions(ctx, resolver, ecosystemTable, args, dispatch.Options{Policy: policyReg, Surface: "ecosystem"})
	})

	// arc-close-filing-review T5: expose each surface's dispatch table
	// over the HTTP daemon's POST /mcp/{surface} route. Mirror of the
	// stdio MCP dispatch — same tables, same policy gate, same default
	// project. Shell hooks (arc-close-filing-review-hook.sh) call this
	// in lieu of stdio. Read-only HTTP callers (the dashboard) are
	// unaffected; the new POST is additive.
	observehttp.RegisterDispatchTable("work", workTable, policyReg, defaultProject, projectPaths)
	observehttp.RegisterDispatchTable("admin", adminTable, policyReg, defaultProject, projectPaths)
	observehttp.RegisterDispatchTable("measure", measureTable, policyReg, defaultProject, projectPaths)
	observehttp.RegisterDispatchTable("knowledge", knowledgeTable, policyReg, defaultProject, projectPaths)
	observehttp.RegisterDispatchTable("ml", mlTable, policyReg, defaultProject, projectPaths)
	// fs retired (T6); sys (introspection only) stays for corpos's delegation.
	observehttp.RegisterDispatchTable("sys", sysTable, policyReg, defaultProject, projectPaths)
	observehttp.RegisterDispatchTable("ecosystem", ecosystemTable, policyReg, defaultProject, projectPaths)

	// SIGHUP triggers a self-re-exec so the post-commit advisor can swap
	// in a rebuilt binary without disconnecting the stdio MCP client.
	// syscall.Exec replaces the process image; stdin/stdout fds survive,
	// the JSON-RPC stream stays open from the client's perspective, and
	// the next request lands in the new binary. See bug 1314 for the
	// design.
	installReExecOnSIGHUP()

	if httpOnly {
		// Block forever — the HTTP server goroutine carries the surface
		// in this mode. Without this, the process would exit immediately
		// after wiring the MCP tools above.
		select {}
	}

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		obs.L().Error("server error", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

// installReExecOnSIGHUP spawns a goroutine that catches SIGHUP and
// re-execs the running binary in place. The new process image inherits
// fds 0/1/2 (stdin/stdout/stderr), so an MCP client talking to this
// process over stdio sees a continuous JSON-RPC stream across the exec
// boundary; the next request lands in the new binary transparently.
//
// Linux-specific (relies on os.Executable resolving via /proc/self/exe
// and syscall.Exec being a true execve). macOS would need a different
// shape but toolkit-server's deployment target is Linux-only today.
//
// API-shape changes (new action, renamed action, new schema) still
// require a full session restart — the MCP client caches the tool/list
// at session init and re-exec doesn't trigger a re-handshake. The
// re-exec covers handler-logic and schema-data changes; it does not
// cover protocol-surface changes. See bug 1267 for the API-rename case.
func installReExecOnSIGHUP() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for sig := range ch {
			path, err := os.Executable()
			if err != nil {
				obs.L().Warn("SIGHUP: os.Executable failed",
					slog.String("err", err.Error()),
					slog.String("ignored_signal", sig.String()),
				)
				continue
			}
			// Resolve symlinks so the exec target is the actual on-disk
			// binary; if the build replaced the path the symlink points
			// to a new inode and we want to exec that one.
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				path = resolved
			}
			obs.L().Info("SIGHUP: re-execing", slog.String("path", path))
			if err := syscall.Exec(path, os.Args, os.Environ()); err != nil {
				// syscall.Exec only returns on failure; on success the
				// process image is replaced and this goroutine ceases
				// to exist. Logging the error is the only useful thing
				// the original process can do.
				obs.L().Error("SIGHUP: syscall.Exec failed",
					slog.String("err", err.Error()))
			}
		}
	}()
}

// healthPingResult is the typed shape for the legacy health_ping shim.
// Drop alongside the shim when the .mcp.json sweep confirms no callers.
type healthPingResult struct {
	OK     bool   `json:"ok"`
	Server string `json:"server"`
}

// loadProjectPaths reads the project registry once at startup. Empty-path
// rows are skipped — they can never match a CWD. The result is sorted by
// path length descending so a longest-prefix scan picks the correct entry
// even when one project's path is a parent of another (e.g. /home/user/dev
// vs /home/user/dev/mcp-servers). The resolver itself lives in the dispatch
// package ([dispatch.NewCwdProjectResolver]) so the native stdio path and the
// HTTP path share one implementation.
func loadProjectPaths(pool *db.Pool) ([]dispatch.ProjectPath, error) {
	rows, err := pool.DB().Query(`SELECT id, path FROM projects WHERE path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dispatch.ProjectPath
	for rows.Next() {
		var p dispatch.ProjectPath
		if err := rows.Scan(&p.ID, &p.Path); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Longest path first so prefix matching is deterministic.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if len(out[j].Path) > len(out[i].Path) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// chainNotifiers composes multiple AfterCreateNotifiers. Nil entries are
// skipped (so callers can build the list unconditionally and let
// optional notifiers no-op themselves). The first error short-circuits;
// later notifiers don't run on a prior failure — matches the existing
// post-commit error-surfacing behaviour in forge.Create where the
// returned envelope reports after_create_error and callers retry the
// secondary effect (FTS5 sync, SSE emit) without re-creating.
//
// arcFallbackForge dispatches a single arcreview unreviewed-fallback forge call
// (memory or vault-note) through the SAME construct create path the forge action
// uses — construct.HandleForgeCreate — so the fallback gets identical persistence +
// envelope behavior (vault-note's pointer upsert via the full AfterCreate
// notifier included). params is the {schema_name, slug, fields} envelope
// PrepareForge already understands. (Chain 311 T7 Stage 6 P2-C.2: forge archived;
// vault-note re-homed into construct, so the fallback no longer needs forge.)
func arcFallbackForge(ctx context.Context, deps, sseFinalize, fullFinalize construct.Deps, project string, params json.RawMessage) error {
	res, err := construct.HandleForgeCreate(ctx, deps, sseFinalize, fullFinalize, project, params)
	if err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("arcreview fallback forge: %s", res.Error)
	}
	return nil
}

// batchForgeCreateInTx builds the TableDeps.ForgeCreateInTx seam: decode a batch
// forge-create op's params, validate via construct.PrepareForge, and route the
// create through construct.CreateInTx on the outer batch tx. Batch's create
// allowlist is the event-sourced db schemas (bug/suggestion/task), all routable;
// a non-routable schema (no InputFromForge arm) errors and rolls back the tx
// (the forge in-tx fallback retired with the archive). Returns the cascade event
// id or a Go error.
func batchForgeCreateInTx(pool *db.Pool, schemaReg *registry.Registry, _ *eventbus.Bus) func(context.Context, *sql.Tx, string, json.RawMessage) (string, error) {
	return func(ctx context.Context, tx *sql.Tx, project string, rawParams json.RawMessage) (string, error) {
		// Scope gate (formerly enforced inside forge.HandleForgeInTx): batch
		// forge-create is scoped to the batch-creatable schemas (bug/suggestion/
		// task). Peek schema_name BEFORE full validation so an out-of-scope schema
		// rejects on scope (not on a missing required field). A chain-with-tasks is
		// served by forge(chain, tasks=[...]) directly, not by batching forge(chain).
		if name := peekForgeSchemaName(rawParams); name != "" && !construct.BatchEligible(name) {
			return "", fmt.Errorf("forge(%s) is not batch-creatable — batch forge create is scoped to bug/suggestion/task; create a chain with its tasks via forge(chain, tasks=[...]) directly", name)
		}
		deps := construct.Deps{Pool: pool, Schemas: schemaReg}
		prep, rej, err := construct.PrepareForge(deps, project, rawParams)
		if err != nil {
			return "", err
		}
		if rej != nil {
			return "", errors.New(rej.Error)
		}
		in, err := construct.InputFromForge(prep)
		if err != nil {
			return "", err
		}
		return construct.CreateInTx(ctx, tx, deps, prep.SchemaName, project, in, prep.Validated)
	}
}

// peekForgeSchemaName extracts schema_name (or its `kind` alias) from a raw
// forge envelope without full parsing — used by the batch create seam's scope
// gate so an out-of-scope schema rejects before required-field validation.
func peekForgeSchemaName(raw json.RawMessage) string {
	var peek struct {
		SchemaName string `json:"schema_name"`
		Kind       string `json:"kind"`
	}
	_ = json.Unmarshal(raw, &peek)
	if peek.SchemaName != "" {
		return peek.SchemaName
	}
	return peek.Kind
}

// batchForgeEditInTx builds the TableDeps.ForgeEditInTx seam: route the db-target
// event-sourced edits (bug/suggestion/chain/task — batch's edit scope) through
// construct.UpdateInTx on the outer batch tx. Markdown-target + delta schemas are
// not batch-aware (their non-tx file/generic writes can't ride the outer tx), so
// they reject here — preserving the pre-archive "not batch-aware in v1" boundary
// (the forge in-tx fallback that enforced it is gone).
func batchForgeEditInTx(pool *db.Pool, schemaReg *registry.Registry) func(context.Context, *sql.Tx, string, json.RawMessage) (string, error) {
	dbEditCovered := map[string]bool{"bug": true, "suggestion": true, "chain": true, "task": true}
	return func(ctx context.Context, tx *sql.Tx, project string, rawParams json.RawMessage) (string, error) {
		deps := construct.Deps{Pool: pool, Schemas: schemaReg}
		prep, rej, err := construct.PrepareForgeEdit(deps, project, rawParams)
		if err != nil {
			return "", err
		}
		if rej != nil {
			return "", errors.New(rej.Error)
		}
		if !dbEditCovered[prep.SchemaName] {
			return "", fmt.Errorf("forge_edit in batch: schema %q is not batch-aware (markdown/delta edits do non-transactional writes that can't ride the batch tx)", prep.SchemaName)
		}
		return construct.UpdateInTx(ctx, tx, deps, prep.SchemaName, project, prep.Slug, prep.ChainSlug, prep.Validated)
	}
}

// handleRecordOrSugar is the work-table `record` action's dispatch body for T7
// Stage 5 (P1-D). It branches on the input shape:
//   - forge-shaped sugar ({schema_name|kind, slug, fields[, op]}) → routes through
//     the construct umbrella via the same adapters the forge / forge_edit /
//     forge_delete actions use (op ∈ create|update|delete, default create). This
//     is the surviving agent write surface once forge archives at Stage 6.
//   - raw {events[...]} → work.HandleRecord (the event-ledger path, kept for
//     lifecycle events + multi-event sequences + dry_run).
//
// Returns json.RawMessage so a single dispatch entry can carry the four distinct
// result envelopes (create/edit/delete/record) without a bare `any` in main.
// The `op` selector is stripped before the params reach the forge prep (which
// would otherwise reject it as an unknown key).
func handleRecordOrSugar(ctx context.Context, recordDeps work.TableDeps, deps, createSSEFinalize, createFullFinalize, editFinalize construct.Deps, project string, params json.RawMessage) (json.RawMessage, error) {
	var peek struct {
		SchemaName string `json:"schema_name"`
		Kind       string `json:"kind"`
		Op         string `json:"op"`
	}
	_ = json.Unmarshal(params, &peek)
	schema := peek.SchemaName
	if schema == "" {
		schema = peek.Kind
	}

	// Raw events[] mode — no schema_name/kind envelope.
	if schema == "" {
		res, err := work.HandleRecord(ctx, recordDeps, project, params)
		return marshalResult(res, err)
	}

	// Forge-shaped sugar. Strip the `op` selector so the prep (which has no `op`
	// param) doesn't reject it as unknown.
	cleaned, err := stripParamKey(params, "op")
	if err != nil {
		return nil, err
	}
	switch peek.Op {
	case "", "create":
		res, err := construct.HandleForgeCreate(ctx, deps, createSSEFinalize, createFullFinalize, project, cleaned)
		return marshalResult(res, err)
	case "update":
		res, err := construct.HandleForgeEdit(ctx, deps, editFinalize, project, cleaned)
		return marshalResult(res, err)
	case "delete":
		res, err := construct.HandleForgeDelete(ctx, deps, project, cleaned)
		return marshalResult(res, err)
	default:
		return json.Marshal(map[string]string{
			"error": fmt.Sprintf("record: op %q invalid — use create|update|delete (or omit for create)", peek.Op),
			"hint":  "record's forge-shaped sugar mode takes {schema_name, slug, fields, op?}; raw event submission takes {events:[…]}.",
		})
	}
}

// marshalResult marshals a sugar/record sub-result to json.RawMessage, propagating
// any hard (Go) error. Envelope-level errors (result.Error) ride inside the
// marshaled bytes, matching every other work action's failure shape.
func marshalResult[T any](res T, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// stripParamKey returns params with the named top-level key removed (used to peel
// record-sugar's `op` selector before the forge prep sees the envelope). A nil /
// empty params round-trips to an empty object.
func stripParamKey(params json.RawMessage, key string) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if len(params) > 0 {
		if err := json.Unmarshal(params, &m); err != nil {
			return nil, fmt.Errorf("record: parse params: %w", err)
		}
	}
	delete(m, key)
	return json.Marshal(m)
}

// Action verb composition: the first non-empty action verb wins. In
// practice IndexUpsertNotifier is the only notifier that returns a
// non-default verb today (chain `forge-vault-note-schema-rework` T4),
// so the composition rule is rarely exercised — but we resolve
// determinism explicitly: insertion order = priority.
func chainNotifiers(notifiers ...construct.AfterCreateNotifier) construct.AfterCreateNotifier {
	return func(ctx context.Context, schemaName, project, slug string, result construct.CreatePersistResult, fields map[string]fieldvalue.FieldValue) (string, error) {
		var action string
		for _, n := range notifiers {
			if n == nil {
				continue
			}
			verb, err := n(ctx, schemaName, project, slug, result, fields)
			if err != nil {
				if action == "" {
					action = verb
				}
				return action, err
			}
			if action == "" && verb != "" {
				action = verb
			}
		}
		return action, nil
	}
}

// makeOnCreateNotifier returns a construct.AfterCreateNotifier that publishes
// an SSE event matching the Rust EventBus variants. bug → BugFiled with
// the supplied severity (defaults to "medium"); chain/task and any
// other schema → ArtifactCreated. Nil bus means no notifier.
func makeOnCreateNotifier(bus *eventbus.Bus) construct.AfterCreateNotifier {
	if bus == nil {
		return nil
	}
	return func(_ context.Context, schemaName, project, slug string, _ construct.CreatePersistResult, fields map[string]fieldvalue.FieldValue) (string, error) {
		switch schemaName {
		case "bug":
			severity := "medium"
			if v, ok := fields["severity"]; ok && !v.IsList && v.Single != "" {
				severity = v.Single
			}
			bus.Publish(eventbus.BugFiled(project, slug, severity))
		case "suggestion":
			priority := "medium"
			if v, ok := fields["priority"]; ok && !v.IsList && v.Single != "" {
				priority = v.Single
			}
			bus.Publish(eventbus.SuggestionFiled(project, slug, priority))
		default:
			bus.Publish(eventbus.ArtifactCreated(project, schemaName, slug))
		}
		// SSE-publish notifier has no opinion on the action verb; let
		// downstream notifiers (IndexUpsertNotifier) decide.
		return "", nil
	}
}

// stampMCPSessionID derives a stable per-MCP-session identifier from
// req.Session and attaches it to ctx via events.WithMCPSessionID. The
// id survives across every tools/call within one stdio (or HTTP)
// connection: ServerSession.ID() if the transport reports one
// (streamable HTTP returns the Mcp-Session-Id header value), otherwise
// the session pointer address (stable for the connection's lifetime).
//
// Downstream callers that want session-scoped state (parse_context's
// filter cache being the load-bearing example) read this value rather
// than the per-call span TraceID. The dispatcher mints a fresh root
// span every call, so a span-derived session id changes per call and
// fails to cache. Fix for chain parse-context-lean-orienting T1
// (cache_hits=0 on identical re-resolutions).
//
// When req or req.Session is nil (tests, unusual transport states),
// returns ctx unchanged — the handler falls through to span TraceID
// (legacy behavior) or skips caching entirely.
func stampMCPSessionID(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Session == nil {
		return ctx
	}
	id := req.Session.ID()
	if id == "" {
		// Stdio transports don't implement hasSessionID, so
		// ServerSession.ID() returns "". The pointer address is stable
		// for the session's lifetime — same connection → same pointer →
		// same key. Prefix with "stdio-" so the value is visually
		// distinguishable from a transport-reported id in logs/traces.
		id = fmt.Sprintf("stdio-%p", req.Session)
	}
	return events.WithMCPSessionID(ctx, id)
}

// stampMCPActor reads the connecting client's identity off the MCP
// initialize handshake and stamps an [events.Actor] onto ctx so the
// dispatch-layer rationale gate and the events.Emit substrate can both
// see it. stdio MCP transport always produces actor.kind = "agent" — the
// portal HTTP write surface (not currently exposed through the MCP
// AddTool callbacks) would stamp human; CLI subcommands stamp system
// elsewhere.
//
// When ClientInfo is missing (an MCP client that skipped or malformed
// the initialize handshake), the actor degrades to
// {kind: "agent", id: "unknown-stdio-client"} — preferable to NULL since
// the dispatch gate still wants to enforce rationale on agent transports,
// and the audit query "show me every emit by an unknown agent" stays
// useful as a discoverability signal.
func stampMCPActor(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Session == nil {
		return events.WithActor(ctx, events.Actor{Kind: "agent", ID: "unknown-stdio-client"})
	}
	id := "unknown-stdio-client"
	if init := req.Session.InitializeParams(); init != nil && init.ClientInfo != nil {
		ci := init.ClientInfo
		if ci.Name != "" && ci.Version != "" {
			id = ci.Name + "-" + ci.Version
		} else if ci.Name != "" {
			id = ci.Name
		}
	}
	return events.WithActor(ctx, events.Actor{Kind: "agent", ID: id})
}

// spanEventsRetention is how long span_events rows are kept before the
// janitor prunes them. 24 h is long enough for any "what happened
// overnight" investigation but short enough that the table stays small
// under high agent traffic.
const spanEventsRetention = 24 * time.Hour

// spanEventsJanitorInterval is the prune cadence. 5 minutes amortises
// over the ~5–20 inserts per tools/call; tighter intervals would just
// burn write-lock contention with no observable retention benefit.
const spanEventsJanitorInterval = 5 * time.Minute

// spanEventsJanitor runs forever, pruning rows older than
// spanEventsRetention every spanEventsJanitorInterval. Owned by the HTTP
// daemon process only so stdio MCPs aren't fighting for the write lock.
func spanEventsJanitor(tail *obs.SpanTail) {
	ticker := time.NewTicker(spanEventsJanitorInterval)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		deleted, err := tail.PruneOlderThan(ctx, time.Now().Add(-spanEventsRetention))
		cancel()
		if err != nil {
			obs.L().Warn("span_events_prune_failed", slog.String("err", err.Error()))
			continue
		}
		if deleted > 0 {
			obs.L().Info("span_events_pruned", slog.Int64("rows", deleted))
		}
	}
}

// countRubrics returns the number of rubrics loaded, keyed by their names.
// Used only for the startup log message.
func countRubrics(reg *rubric.Registry) int {
	count := 0
	for _, name := range []string{
		"chain-assessment", "retirement-signal", "tiered-context",
		"agentic-audit", "artifact-review", "session-routing",
		"pre-commit-failure", "docstring-drift",
		"refactoring-heuristics", "pre-context-summarization",
	} {
		if _, ok := reg.Get(name); ok {
			count++
		}
	}
	return count
}

// Meta-tool descriptions surfaced to MCP clients. Hand-kept constants
// in package internal/actiondocs; the parity test at
// meta_tool_descriptions_test.go asserts the action lists stay in sync
// with the action-docs corpus at go/internal/actiondocs/corpus/<surface>/.
// See actiondocs.WorkDescription etc. for the source-of-truth.
