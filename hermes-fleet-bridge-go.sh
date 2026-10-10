#!/bin/bash
# hermes-fleet-bridge-go.sh — run Go commands inside Docker (build/test/vet/
# tidy/fmt) with a persistent cache so repeated runs are fast and low-RAM.
#
# Usage:
#   ./hermes-fleet-bridge-go.sh <go args...>   e.g. ./hermes-fleet-bridge-go.sh test -race ./...
#   ./hermes-fleet-bridge-go.sh vet ./...
#   ./hermes-fleet-bridge-go.sh mod tidy
#   ./hermes-fleet-bridge-go.sh gofmt -l .      (direct gofmt binary, for the CI fmt gate)
#
# Caches (bind-mounted, NOT named volumes — per the deployment rule):
#   gomod   = ~/.cache/hermes-fleet-bridge-go/mod     module download cache (/go/pkg/mod)
#   gobuild = ~/.cache/hermes-fleet-bridge-go/build   compiled package cache (/root/.cache/go-build)
#
# Toolchain image is pinned to the same Go version as the Dockerfile build
# stage (golang:1.23). It uses the Debian variant (not alpine) so cgo is
# available and `go test -race` works; the Dockerfile itself still builds the
# shipped binary with CGO_ENABLED=0 on alpine. Override with HFB_GO_IMAGE.
#
# RAM safety:
#   --memory/-m limits the container's hard memory ceiling (OOM inside, not host OOM)
#   --memory-swap = same value => no swap growth for this container
#   GOFLAGS=-p=2 caps compiler parallelism to cut peak RAM further.

set -eu

GO_IMAGE="${HFB_GO_IMAGE:-golang:1.23-bookworm}"
CACHE_DIR="${HFB_GO_CACHE:-$HOME/.cache/hermes-fleet-bridge-go}"
MOD_CACHE="$CACHE_DIR/mod"
BUILD_CACHE="$CACHE_DIR/build"
MEM_LIMIT="${HFB_GO_MEM:-2g}"   # hard RAM cap for the container (-race needs headroom)

# Create cache dirs if absent
mkdir -p "$MOD_CACHE" "$BUILD_CACHE"

# Repo root = this script's own directory (it lives next to go.mod). Resolve
# symlinks and get the absolute path.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT="$SCRIPT_DIR"

# A container without a TTY/terminal shouldn't try to allocate one.
TTY_FLAG=""
if [ -t 1 ]; then TTY_FLAG="-it"; fi

ENV_FLAGS="-e GOFLAGS=-p=2"

# Log capture: always write full output to a repo-local log file under
# tests/logs/ (gitignored, persistent across runs). The summary printed at
# the end shows the path; on failure the first FAIL line is also inlined.
if [ -n "${HFB_TEST_DIR:-}" ]; then
    LOG_DIR="$HFB_TEST_DIR"
else
    LOG_DIR="tests/logs/hermes-fleet-bridge-go"
fi
mkdir -p "$LOG_DIR"
if [ -n "${HFB_TEST_DIR:-}" ]; then
    LOG_FILE="$LOG_DIR/out.log"
else
    LOG_SLUG=$(echo "$*" | tr ' /' '__')
    LOG_FILE="$LOG_DIR/$(date -u +%Y%m%dT%H%M%S)-go-${LOG_SLUG:0:120}-$$.log"
fi
ts=$(date +%s)

# gofmt passthrough: `go fmt` is not `gofmt -l`, and the CI gate checks the
# latter, so run the gofmt binary directly when asked.
if [ "${1:-}" = "gofmt" ]; then
    docker run $TTY_FLAG --rm \
        --memory="$MEM_LIMIT" \
        --memory-swap="$MEM_LIMIT" \
        $ENV_FLAGS \
        -v "$MOD_CACHE:/go/pkg/mod" \
        -v "$BUILD_CACHE:/root/.cache/go-build" \
        -v "$REPO_ROOT:/src" \
        -w /src \
        "$GO_IMAGE" \
        gofmt "${@:2}" 2>&1 | tee "$LOG_FILE"
else
    docker run $TTY_FLAG --rm \
        --memory="$MEM_LIMIT" \
        --memory-swap="$MEM_LIMIT" \
        $ENV_FLAGS \
        -v "$MOD_CACHE:/go/pkg/mod" \
        -v "$BUILD_CACHE:/root/.cache/go-build" \
        -v "$REPO_ROOT:/src" \
        -w /src \
        "$GO_IMAGE" \
        go "$@" 2>&1 | tee "$LOG_FILE"
fi
rc=${PIPESTATUS[0]}
te=$(date +%s)

echo "==> hermes-fleet-bridge-go.sh: rc=$rc, $((te - ts))s elapsed, log: $LOG_FILE" >&2
if [ "$rc" -ne 0 ]; then
    first_fail=$(grep -m1 -E '^--- FAIL|^FAIL\b' "$LOG_FILE" 2>/dev/null | head -c 400 || true)
    [ -n "$first_fail" ] && echo "    first failure: $first_fail" >&2
fi
exit "$rc"
