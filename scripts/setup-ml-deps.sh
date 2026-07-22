#!/usr/bin/env bash
#
# setup-ml-deps.sh — install the ONNX Runtime native library
# (`libonnxruntime.so`) for the go/internal/ml/ subsystem.
#
# Downloads the official Microsoft ONNX Runtime CPU-only release to
# `<repo-root>/vendor/onnxruntime/lib/libonnxruntime.so` and configures
# the convention `ml.InitializeONNXRuntime` probes by default.
#
# Usage:
#   scripts/setup-ml-deps.sh             # download default version
#   ORT_VERSION=1.20.0 scripts/setup-ml-deps.sh
#
# Idempotent: re-running is a no-op once the binary is in place.

set -euo pipefail

readonly DEFAULT_ORT_VERSION="1.20.1"
ORT_VERSION="${ORT_VERSION:-$DEFAULT_ORT_VERSION}"

# Resolve repo root by walking up from this script's location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
VENDOR_DIR="$REPO_ROOT/vendor/onnxruntime"
LIB_DEST="$VENDOR_DIR/lib/libonnxruntime.so"

# Platform detection. ML serving is CPU-only by commitment (see
# docs/ML_CAPABILITY_SUBSTRATE.md §3.2); we always pull the CPU release.
UNAME_S="$(uname -s)"
UNAME_M="$(uname -m)"
case "$UNAME_S-$UNAME_M" in
    Linux-x86_64)
        PLATFORM="linux-x64"
        ;;
    Linux-aarch64|Linux-arm64)
        PLATFORM="linux-aarch64"
        ;;
    Darwin-x86_64)
        PLATFORM="osx-x86_64"
        ;;
    Darwin-arm64)
        PLATFORM="osx-arm64"
        ;;
    *)
        echo "setup-ml-deps: unsupported platform $UNAME_S-$UNAME_M" >&2
        echo "Download the matching ONNX Runtime release manually from" >&2
        echo "https://github.com/microsoft/onnxruntime/releases/tag/v$ORT_VERSION" >&2
        echo "and extract libonnxruntime.so to $LIB_DEST" >&2
        exit 1
        ;;
esac

ARCHIVE="onnxruntime-$PLATFORM-$ORT_VERSION.tgz"
URL="https://github.com/microsoft/onnxruntime/releases/download/v$ORT_VERSION/$ARCHIVE"

if [[ -f "$LIB_DEST" ]]; then
    echo "setup-ml-deps: $LIB_DEST already present (ORT $ORT_VERSION); skipping download"
    echo "setup-ml-deps: remove $VENDOR_DIR and re-run to refresh"
    exit 0
fi

echo "setup-ml-deps: downloading ONNX Runtime $ORT_VERSION for $PLATFORM"
mkdir -p "$VENDOR_DIR"
TMPDIR_LOCAL="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_LOCAL"' EXIT

curl --fail --location --output "$TMPDIR_LOCAL/$ARCHIVE" "$URL"
tar -xzf "$TMPDIR_LOCAL/$ARCHIVE" -C "$TMPDIR_LOCAL"

EXTRACTED_DIR="$TMPDIR_LOCAL/onnxruntime-$PLATFORM-$ORT_VERSION"
if [[ ! -d "$EXTRACTED_DIR" ]]; then
    echo "setup-ml-deps: extracted dir not found at $EXTRACTED_DIR" >&2
    ls -la "$TMPDIR_LOCAL" >&2
    exit 1
fi

# The ORT release ships lib/ + include/ + LICENSE. We only need lib/ for
# serving (the include/ headers matter only for static linking, which
# the Go binding doesn't do).
mkdir -p "$VENDOR_DIR/lib"
cp -a "$EXTRACTED_DIR/lib/." "$VENDOR_DIR/lib/"
cp -a "$EXTRACTED_DIR/LICENSE" "$VENDOR_DIR/" 2>/dev/null || true
cp -a "$EXTRACTED_DIR/VERSION_NUMBER" "$VENDOR_DIR/" 2>/dev/null || true

# Some releases ship libonnxruntime.so as a symlink to libonnxruntime.so.X.Y.Z.
# Verify the canonical name is reachable.
if [[ ! -e "$LIB_DEST" ]]; then
    # Try to find any libonnxruntime.so.* and link the unsuffixed name.
    versioned="$(find "$VENDOR_DIR/lib" -maxdepth 1 -name 'libonnxruntime.so.*' -printf '%f\n' | head -1)"
    if [[ -n "$versioned" ]]; then
        ln -sf "$versioned" "$LIB_DEST"
    fi
fi
if [[ ! -e "$LIB_DEST" ]]; then
    echo "setup-ml-deps: failed to land libonnxruntime.so at $LIB_DEST" >&2
    ls -la "$VENDOR_DIR/lib" >&2
    exit 1
fi

echo "setup-ml-deps: installed ONNX Runtime $ORT_VERSION at $LIB_DEST"
echo
echo "Verify by running:"
echo "  make -C $REPO_ROOT/go test ./internal/ml/..."
echo
echo "The integration test runtime_integration_test.go probes for"
echo "$LIB_DEST and runs the load→infer→unload loop end-to-end."
