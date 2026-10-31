#!/usr/bin/env sh
# 交叉编译 filet。优先使用项目内的独立工具链（.toolchain/go），不动系统 Go。
set -eu
cd "$(dirname "$0")/.."
PROJ="$(pwd)"

if [ -x "$PROJ/.toolchain/go/bin/go" ]; then
  export GOROOT="$PROJ/.toolchain/go"
  export PATH="$PROJ/.toolchain/go/bin:$PATH"
fi

export CGO_ENABLED=0
export GOTOOLCHAIN=local
export GOMODCACHE="${GOMODCACHE:-$PROJ/.cache/gomod}"
export GOCACHE="${GOCACHE:-$PROJ/.cache/go-build}"

VERSION="${VERSION:-dev}"
OUT="$PROJ/dist"
mkdir -p "$OUT"

build() {
  os="$1"
  arch="$2"
  ext="$3"
  echo "==> $os/$arch"
  GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$OUT/filet-$os-$arch$ext" ./cmd/filet
}

build linux amd64 ""
build linux arm64 ""
build windows amd64 ".exe"
build darwin amd64 ""
build darwin arm64 ""

echo
ls -lh "$OUT"
