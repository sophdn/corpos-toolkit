//go:build !cgo

package ml

import "errors"

// ErrMLUnavailableNoCGO is returned by the ml runtime entry points in a
// CGO_ENABLED=0 build. The ONNX inference runtime (github.com/yalue/
// onnxruntime_go) is a cgo binding, so a CGo-free static build (the
// distroless/static container target) has no in-process inference. The
// ml surface still registers — its actions fail closed with this error
// rather than the binary failing to link — which is exactly the behavior
// once inference moves to the onnx-serve sidecar (terminal topology).
var ErrMLUnavailableNoCGO = errors.New("ml: ONNX inference unavailable — binary built without cgo (CGO_ENABLED=0); the onnxruntime_go binding is excluded from this build. Run the cgo build for in-process inference, or call the onnx-serve sidecar")

// InitializeONNXRuntime is the !cgo stub: there is no in-process ONNX
// runtime to initialize, so it always reports unavailable.
func InitializeONNXRuntime(libPath string) error {
	return ErrMLUnavailableNoCGO
}

// IsONNXRuntimeInitialized is always false in a CGo-free build — there is
// no onnxruntime to initialize.
func IsONNXRuntimeInitialized() bool {
	return false
}

// realSessionFactory is the !cgo stub SessionFactory wired into Registry
// by default (registry.go). Any attempt to build an inference session
// fails closed with ErrMLUnavailableNoCGO.
func realSessionFactory(modelPath, inputName, outputName string) (Session, error) {
	return nil, ErrMLUnavailableNoCGO
}
