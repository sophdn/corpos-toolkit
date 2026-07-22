//go:build cgo

package ml

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// onnxRuntimeState tracks the global ORT initialization state. ORT's
// InitializeEnvironment is process-wide; calling it twice panics. The
// mutex + initialized flag keep it idempotent across registry instances.
type onnxRuntimeState struct {
	mu          sync.Mutex
	initialized bool
	libPath     string
}

var ortState onnxRuntimeState

// InitializeONNXRuntime configures the shared library path and
// initializes the ORT environment. Idempotent — subsequent calls are
// no-ops if libPath matches the first call's path. Returns an error if
// libPath is "" (no library configured) or if the binding's
// InitializeEnvironment fails.
//
// libPath resolution order:
//  1. The explicit argument (if non-empty).
//  2. The ML_ONNXRUNTIME_LIB_PATH env var.
//  3. The conventional project-local path
//     `<repo-root>/vendor/onnxruntime/lib/libonnxruntime.so` if it
//     exists on disk.
//
// scripts/setup-ml-deps.sh downloads the binary to option (3)'s path
// by default.
func InitializeONNXRuntime(libPath string) error {
	ortState.mu.Lock()
	defer ortState.mu.Unlock()

	resolved := libPath
	if resolved == "" {
		resolved = os.Getenv("ML_ONNXRUNTIME_LIB_PATH")
	}
	if resolved == "" {
		// Probe the conventional vendor location.
		cwd, err := os.Getwd()
		if err == nil {
			candidate := filepath.Join(cwd, "vendor", "onnxruntime", "lib", "libonnxruntime.so")
			if _, statErr := os.Stat(candidate); statErr == nil {
				resolved = candidate
			}
		}
	}
	if resolved == "" {
		return ErrONNXRuntimeNotInitialized
	}

	if ortState.initialized {
		if ortState.libPath != resolved {
			return fmt.Errorf("onnxruntime already initialized with %q; cannot re-init with %q", ortState.libPath, resolved)
		}
		return nil
	}

	ort.SetSharedLibraryPath(resolved)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort.InitializeEnvironment failed (lib=%q): %w", resolved, err)
	}
	ortState.initialized = true
	ortState.libPath = resolved
	return nil
}

// IsONNXRuntimeInitialized reports whether the ORT environment has been
// set up. Tests use this to t.Skip() the integration path cleanly.
func IsONNXRuntimeInitialized() bool {
	ortState.mu.Lock()
	defer ortState.mu.Unlock()
	return ortState.initialized
}

// ortSession is the production Session implementation, wrapping a
// onnxruntime_go DynamicAdvancedSession. One ortSession per loaded
// (model, version) — Registry caches these.
type ortSession struct {
	impl       *ort.DynamicAdvancedSession
	inputName  string
	outputName string
}

func newOrtSession(modelPath, inputName, outputName string) (Session, error) {
	if !IsONNXRuntimeInitialized() {
		return nil, ErrONNXRuntimeNotInitialized
	}
	impl, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{inputName}, []string{outputName}, nil)
	if err != nil {
		return nil, fmt.Errorf("NewDynamicAdvancedSession(%q): %w", modelPath, err)
	}
	return &ortSession{impl: impl, inputName: inputName, outputName: outputName}, nil
}

func (s *ortSession) Run(inputData []float32, inputShape []int64) ([]float32, []int64, error) {
	input, err := ort.NewTensor(ort.NewShape(inputShape...), inputData)
	if err != nil {
		return nil, nil, fmt.Errorf("NewTensor: %w", err)
	}
	defer input.Destroy()

	// Letting onnxruntime allocate the output tensor for us — the
	// model's output shape is dynamic for the cases we care about.
	outputs := []ort.Value{nil}
	if err := s.impl.Run([]ort.Value{input}, outputs); err != nil {
		return nil, nil, fmt.Errorf("session.Run: %w", err)
	}
	defer outputs[0].Destroy()

	tensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, nil, fmt.Errorf("output tensor is not float32 (got %T)", outputs[0])
	}
	// Copy out of the tensor's backing buffer — the buffer is owned by
	// onnxruntime and freed by Destroy. Returning a slice into it would
	// be a use-after-free.
	src := tensor.GetData()
	out := make([]float32, len(src))
	copy(out, src)

	shape := tensor.GetShape()
	outShape := make([]int64, len(shape))
	copy(outShape, shape)

	return out, outShape, nil
}

func (s *ortSession) Close() error {
	if s.impl == nil {
		return nil
	}
	s.impl.Destroy()
	s.impl = nil
	return nil
}

// realSessionFactory is the production SessionFactory passed to
// Registry by the BuildTable seam. Tests inject a fakeFactory.
func realSessionFactory(modelPath, inputName, outputName string) (Session, error) {
	return newOrtSession(modelPath, inputName, outputName)
}
