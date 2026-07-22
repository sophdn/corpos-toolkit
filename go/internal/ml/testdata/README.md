# go/internal/ml/testdata

Fixtures for the `internal/ml` integration tests.

## `example_dynamic_axes.onnx`

Vendored from [github.com/yalue/onnxruntime_go](https://github.com/yalue/onnxruntime_go) at v1.30.1, `test_data/example_dynamic_axes.onnx` (MIT-licensed, Copyright (c) 2023 Nathan Otterness). 320-byte ONNX model with the contract:

- Input tensor `input_vectors` of shape `(batch, 10)` float32.
- Output tensor `output_scalars` of shape `(batch,)` float32.

The model takes a dynamic batch size of 10-element vectors and returns a scalar per row. Used to exercise the load → infer → unload loop end-to-end through `onnxruntime_go.DynamicAdvancedSession` without inflating CI time or repo size.

## Regenerating

The original generator script (`generate_network_dynamic_axes.py`) lives upstream in `yalue/onnxruntime_go`'s test infra. If you need a different shape for a new test, prefer copying another fixture from upstream over hand-rolling — the upstream fixtures are known-good against the same ORT version this project pins.

## Running the integration test

The fixture alone isn't enough — `runtime_integration_test.go` also needs `libonnxruntime.so` available. From the repo root:

```bash
scripts/setup-ml-deps.sh
make -C go test ./internal/ml/...
```

Without the lib, the integration test skips cleanly via `t.Skip()`. The fakeSession-backed unit tests in `inference_test.go` run unconditionally.
