#!/bin/sh
# Source this before go commands. Keeps toolchain, caches and temp files on E:.
PROJ="E:/qoderProject/fileT"
PROJ_MSYS="/e/qoderProject/fileT"
export GOROOT="$PROJ/.toolchain/go"
export PATH="$PROJ_MSYS/.toolchain/go/bin:$PATH"
export GOPATH="$PROJ/.cache/gopath"
export GOMODCACHE="$PROJ/.cache/gomod"
export GOCACHE="$PROJ/.cache/go-build"
export GOENV="$PROJ/.cache/goenv"
export GOTELEMETRY="off"
export TMP="$PROJ/.tmp"
export TEMP="$TMP"
export TMPDIR="$TMP"
export GOPROXY="https://goproxy.cn,direct"
export GOTOOLCHAIN="local"
export CGO_ENABLED=0
