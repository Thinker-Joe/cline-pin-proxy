#!/usr/bin/env bash
#
# Run formatting, vet, build, and race checks in a Linux container.
# Required before pushing errno, file-permission, or path-handling changes.
# ENOTSUP and EOPNOTSUPP, for example, are aliases on Linux but not Windows.
#
# Usage: bash scripts/linux-check.sh
# Requires Docker and network access for the image and Alpine C toolchain.
# GO_IMAGE may select another compatible Alpine Go image.
#
set -euo pipefail

cd "$(dirname "$0")/.."

GO_IMAGE="${GO_IMAGE:-golang:1.25-alpine}"

echo "=== 在 $GO_IMAGE 里跑 gofmt / vet / build / test -race ==="

docker run --rm \
  -v "$PWD":/app -w /app \
  "$GO_IMAGE" sh -eu -c '
    # The race detector requires cgo and a C compiler.
    apk add --no-cache gcc musl-dev >/dev/null 2>&1
    echo "go: $(go version)"

    echo "--- gofmt -l . ---"
    out="$(gofmt -l .)"
    if [ -n "$out" ]; then
      echo "$out"
      echo "FAIL: 以下文件未格式化"
      exit 1
    fi
    echo "clean"

    echo "--- go vet ./... ---"
    go vet ./...

    echo "--- go build ./... ---"
    go build ./...

    echo "--- go test -race ./... ---"
    go test -race -count=1 ./...
  '

echo "=== Linux 验收通过 ==="
