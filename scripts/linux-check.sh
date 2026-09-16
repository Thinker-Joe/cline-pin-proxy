#!/usr/bin/env bash
#
# 在真实 Linux 上跑一遍本项目的验收标准。
#
# 为什么需要它：本项目在 Windows 上开发、在 Linux 上发布，而两者的差异**会
# 直接导致编译失败**，且单靠 Windows 上的 gofmt / go vet 发现不了。真实案例：
#
#   switch errno {
#   case syscall.ENOTSUP, syscall.EOPNOTSUPP:   // Linux 上这是重复 case！
#
# Linux 上 ENOTSUP 与 EOPNOTSUPP 是同一个常量（95），switch 直接编译失败；
# Windows 上它们却是两个不同的值，所以本地一路绿灯，直到 CI 才炸。
#
# 凡是碰到 errno 常量、文件权限语义、路径分隔符的改动，推之前都跑一次这个。
#
# 用法（需要 Docker，任何平台都可以）：
#   bash scripts/linux-check.sh
#
set -euo pipefail

cd "$(dirname "$0")/.."

GO_IMAGE="${GO_IMAGE:-golang:1.25-alpine}"

echo "=== 在 $GO_IMAGE 里跑 gofmt / vet / build / test -race ==="

docker run --rm \
  -v "$PWD":/app -w /app \
  "$GO_IMAGE" sh -eu -c '
    # -race 需要 cgo，alpine 上要装 gcc
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
