#!/usr/bin/env bash
# 在 Linux（或 WSL）上编译：得到一个静态链接的 bin/l5，拷到任何 x86_64 Linux 都能跑。
# 需要 Go 1.22+ 和 gcc（mattn/go-sqlite3 是 cgo，内置 SQLite 3.46 + FTS5）。
set -euo pipefail
cd "$(dirname "$0")"
VER=${VER:-$(date +%Y.%m.%d)}
export CGO_ENABLED=1 GOFLAGS=-mod=mod
go test -tags sqlite_fts5 ./...
go build -trimpath -tags "sqlite_fts5 sqlite_omit_load_extension netgo osusergo" \
  -ldflags "-s -w -X main.version=$VER -extldflags -static" -o bin/l5 ./cmd/l5
echo "bin/l5 $VER"
