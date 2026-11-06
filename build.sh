#!/usr/bin/env bash
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# SyncDelta 构建脚本 —— 在 macOS(自带 bash 3.2)和 Linux 上都能直接运行。
# 纯 Go、无 CGO,所以在一台机器上就能编出所有平台的版本。
#
# 用法:
#   ./build.sh                  构建全部目标:macOS arm64、Windows x64、Linux x64
#   ./build.sh native           只构建当前这台机器对应的版本(最快)
#   ./build.sh mac|win|linux    只构建指定目标,可以写多个,例如 ./build.sh mac win
#   ./build.sh --test [目标]     构建前先运行 go vet 和单元测试
#   VERSION=1.5.0 ./build.sh    指定版本号
#
# 产物在 dist/ 目录,并生成 dist/SHA256SUMS。

set -eu
set -o pipefail

cd "$(dirname "$0")"

VERSION="${VERSION:-1.5.0}"
MIN_GO_MINOR=22   # 需要 Go 1.22+

die()  { echo "错误: $*" >&2; exit 1; }
warn() { echo "提示: $*" >&2; }

usage() {
  sed -n '6,17p' "$0" | sed 's/^# \{0,1\}//'
}

# ---------------------------------------------------------------- 参数
RUN_TESTS=0
TARGETS=""
for a in "$@"; do
  case "$a" in
    --test)        RUN_TESTS=1 ;;
    -h|--help)     usage; exit 0 ;;
    all|mac|win|linux|native) TARGETS="$TARGETS $a" ;;
    *)             die "未知参数: $a(用 ./build.sh --help 查看用法)" ;;
  esac
done
[ -n "$TARGETS" ] || TARGETS="all"

# ---------------------------------------------------------------- 找到 Go
if ! command -v go >/dev/null 2>&1; then
  # 从 Finder/某些终端启动时 PATH 可能不全,试一下常见安装位置
  for d in /opt/homebrew/bin /usr/local/go/bin /usr/local/bin "$HOME/.homebrew/bin" "$HOME/go/bin" "$HOME/sdk/go/bin"; do
    if [ -x "$d/go" ]; then PATH="$d:$PATH"; export PATH; break; fi
  done
fi
command -v go >/dev/null 2>&1 || die "没有找到 Go。请先安装 Go 1.${MIN_GO_MINOR} 或更新版本(macOS: brew install go,或从 https://go.dev/dl/ 下载)。"

gov="$(go env GOVERSION 2>/dev/null || true)"
[ -n "$gov" ] || gov="$(go version | awk '{print $3}')"
ver="${gov#go}"
major="${ver%%.*}"
rest="${ver#*.}"
minor="${rest%%.*}"
case "$major$minor" in
  ''|*[!0-9]*) warn "无法识别 Go 版本 ($gov),跳过版本检查" ;;
  *)
    if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt "$MIN_GO_MINOR" ]; }; then
      die "Go 版本太旧($gov),需要 1.${MIN_GO_MINOR} 或更新版本。"
    fi ;;
esac

# ---------------------------------------------------------------- 展开目标列表
LIST=""
add() { case " $LIST " in *" $1 "*) ;; *) LIST="$LIST $1" ;; esac; }
for t in $TARGETS; do
  case "$t" in
    all)    add darwin/arm64; add windows/amd64; add linux/amd64 ;;
    mac)    add darwin/arm64 ;;
    win)    add windows/amd64 ;;
    linux)  add linux/amd64 ;;
    native) add "$(go env GOOS)/$(go env GOARCH)" ;;
  esac
done

out_name() { # <goos> <goarch>
  case "$1" in
    darwin)  echo "syncdelta-macos-$2" ;;
    windows) echo "syncdelta-windows-$2.exe" ;;
    *)       echo "syncdelta-$1-$2" ;;
  esac
}

# ---------------------------------------------------------------- 检查
echo "Go: $gov   版本号: $VERSION"
if [ "$RUN_TESTS" -eq 1 ]; then
  echo "== go vet"
  go vet ./...
  echo "== go test"
  go test ./...
fi

# ---------------------------------------------------------------- 构建
mkdir -p dist
echo "== 构建"
for t in $LIST; do
  goos="${t%/*}"
  goarch="${t#*/}"
  name="$(out_name "$goos" "$goarch")"
  extra=""
  # Windows 用 GUI 子系统编译:双击运行时不会弹出黑色控制台窗口
  if [ "$goos" = "windows" ]; then extra="-H=windowsgui"; fi
  printf '  %-32s' "$name"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION $extra" -o "dist/$name" .
  if [ "$goos" != "windows" ]; then chmod +x "dist/$name"; fi
  echo "ok"
done

# ---------------------------------------------------------------- 校验和
# 只统计文件:直接在 dist/ 里运行程序会产生 dist/syncdelta-data/ 文件夹,不能把它算进去
SUM_FILES=""
for f in dist/syncdelta-*; do
  if [ -f "$f" ]; then SUM_FILES="$SUM_FILES $(basename "$f")"; fi
done
if [ -n "$SUM_FILES" ]; then
  if command -v shasum >/dev/null 2>&1; then
    # shellcheck disable=SC2086
    (cd dist && shasum -a 256 $SUM_FILES > SHA256SUMS)
  elif command -v sha256sum >/dev/null 2>&1; then
    # shellcheck disable=SC2086
    (cd dist && sha256sum $SUM_FILES > SHA256SUMS)
  fi
fi

echo
ls -lh dist | sed '1d'
case "$(go env GOOS)/$(go env GOARCH)" in
  darwin/arm64) echo; echo "在这台 Mac 上运行: ./dist/syncdelta-macos-arm64" ;;
  linux/amd64)  echo; echo "在这台机器上运行: ./dist/syncdelta-linux-amd64" ;;
esac
