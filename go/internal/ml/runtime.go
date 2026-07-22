package ml

import "errors"

// Session is the inference primitive the inference handler uses. The
// real implementation (runtime_cgo.go) wraps onnxruntime_go.DynamicAdvancedSession;
// fakeSession in tests fulfils the same contract without requiring
// libonnxruntime.so to be installed.
//
// Run takes one input tensor's []float32 values + its shape, dispatches
// through the underlying session, and writes the output tensor's
// []float32 values into out.
//
// Multi-input / multi-output models will gain a wider surface in T5 when
// the MCP convenience-action layer wraps per-task feature marshaling.
// T3 ships single-input / single-output to land the wiring; the
// cross-encoder reranker (two inputs: query + candidate) needs the
// wider surface and will drop here as a follow-on.
type Session interface {
	Run(inputData []float32, inputShape []int64) (output []float32, outputShape []int64, err error)
	Close() error
}

// SessionFactory builds a Session for a given on-disk ONNX file +
// input/output tensor names. Production callers pass realSessionFactory
// (defined per build tag: the real onnxruntime-backed one under cgo, an
// "unavailable" stub under !cgo); tests can pass a fake factory that
// ignores the path and returns a fakeSession with pre-configured behavior.
type SessionFactory func(modelPath, inputName, outputName string) (Session, error)

// ErrONNXRuntimeNotInitialized surfaces from realSessionFactory when
// the ONNX Runtime native library hasn't been wired in via
// InitializeONNXRuntime. The MCP boot path calls InitializeONNXRuntime
// before BuildTable wires ml-backed actions; tests skip when the lib
// path is unset.
var ErrONNXRuntimeNotInitialized = errors.New("onnxruntime not initialized: call ml.InitializeONNXRuntime first (see scripts/setup-ml-deps.sh)")

// --- CGo boundary -----------------------------------------------------------
//
// The ONNX inference runtime is provided by github.com/yalue/onnxruntime_go,
// a cgo binding (deferred dlopen of libonnxruntime.so). That package's files
// carry a `//go:build cgo` constraint, so a CGO_ENABLED=0 build excludes it
// entirely. To keep toolkit-server buildable as a CGo-free static binary
// (distroless/static, the corpos-aligned container target) the onnxruntime-
// touching code is split by build tag:
//
//   - runtime_cgo.go   (//go:build cgo)  — real onnxruntime_go-backed
//     InitializeONNXRuntime / IsONNXRuntimeInitialized / realSessionFactory.
//   - runtime_nocgo.go (//go:build !cgo) — stubs that report the runtime as
//     unavailable; ml-backed actions then fail closed with a clear error
//     instead of the build failing to link.
//
// This is the first concrete step of the terminal topology's "ml leaves the
// core → onnx-serve sidecar" move: a CGO_ENABLED=0 build has no in-process
// ONNX runtime, which is exactly the shape once inference is a sidecar call.
// The native daemon (Makefile CGO_ENABLED=1) keeps full in-process inference.
