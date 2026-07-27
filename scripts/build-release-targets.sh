#!/usr/bin/env bash
# 按指定平台清单交叉编译并打包。
# 用法: bash scripts/build-release-targets.sh <版本号> "<GOOS/GOARCH 列表>"
# 例如: bash scripts/build-release-targets.sh v1.2.0 "windows/amd64 windows/386"
#
# 与 build-release.sh 的区别：本脚本只构建传入的平台，便于 CI 矩阵并行。
# 打包逻辑与产物结构与 build-release.sh 完全一致，确保最终 Release 产物统一。
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"
TARGET_PLATFORMS="${2:-}"

if [ -z "$TARGET_PLATFORMS" ]; then
  echo "错误：未指定目标平台。用法: $0 <版本号> \"<GOOS/GOARCH 列表>\"" >&2
  exit 1
fi

OUT="dist"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.buildCommit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

mkdir -p "$OUT"

# build <goos> <goarch> <binary_name> <package_name> <extra_files...>
build() {
  local goos="$1" goarch="$2" bin="$3" pkg="$4"; shift 4
  local stage="$OUT/$pkg"
  echo "==> 编译 $goos/$goarch"

  if [ "$goos" = "android" ]; then
    if [ -z "${ANDROID_NDK_HOME:-}" ]; then
      echo "错误：未指定 ANDROID_NDK_HOME 环境变量！" >&2
      exit 1
    fi
    local host_os
    case "$(uname -s)" in
      Linux*)               host_os="linux-x86_64";;
      Darwin*)              host_os="darwin-x86_64";;
      MSYS*|MINGW*|CYGWIN*) host_os="windows-x86_64";;
      *)                    host_os="linux-x86_64";;
    esac
    local ext=""
    if [ "$host_os" = "windows-x86_64" ]; then
      ext=".cmd"
    fi
    local clang_cc="${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/${host_os}/bin/aarch64-linux-android28-clang${ext}"
    if [ ! -f "${clang_cc}" ]; then
      echo "错误：找不到 Android NDK 编译器：${clang_cc}" >&2
      exit 1
    fi
    echo "    -> 使用 NDK 编译器: ${clang_cc}"
    CGO_ENABLED=1 GOOS="$goos" GOARCH="$goarch" CC="${clang_cc}" \
      go build -trimpath -ldflags="$LDFLAGS" -o "$stage/$bin" ./cmd/vproxy
  else
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags="$LDFLAGS" -o "$stage/$bin" ./cmd/vproxy
  fi

  mkdir -p "$stage/config"
  cp config/config.example.json   "$stage/config/"
  cp config/api_keys.example.txt  "$stage/config/"
  cp config/models.json           "$stage/config/"
  cp 部署指南.md                   "$stage/"
  cp cmd/vproxy/rules.txt         "$stage/"
  for f in "$@"; do cp "$f" "$stage/"; done

  if command -v zip >/dev/null 2>&1; then
    (cd "$stage" && zip -rq "../$pkg.zip" ./* && cd .. && rm -rf "$pkg")
  elif command -v 7z >/dev/null 2>&1; then
    (cd "$stage" && 7z a -tzip -mx=9 "../$pkg.zip" ./* >/dev/null && cd .. && rm -rf "$pkg")
  else
    echo "错误：找不到 zip 也没有 7z，无法打包" >&2
    exit 1
  fi
  echo "    -> $OUT/$pkg.zip"
}

# 平台 → 打包参数映射表（与 build-release.sh 保持一致）
build_platform() {
  local target="$1"
  case "$target" in
    windows/amd64)
      build windows amd64 vertex-proxy.exe vertex-proxy-windows-amd64 scripts/启动.bat scripts/setup.bat ;;
    windows/386)
      build windows 386   vertex-proxy.exe vertex-proxy-windows-386   scripts/启动.bat scripts/setup.bat ;;
    linux/amd64)
      build linux   amd64 vertex-proxy     vertex-proxy-linux-amd64   scripts/start.sh scripts/vertex-proxy.service scripts/setup.sh ;;
    linux/386)
      build linux   386   vertex-proxy     vertex-proxy-linux-386     scripts/start.sh scripts/vertex-proxy.service scripts/setup.sh ;;
    linux/arm64)
      build linux   arm64 vertex-proxy     vertex-proxy-linux-arm64   scripts/start.sh scripts/vertex-proxy.service scripts/setup.sh ;;
    linux/arm)
      build linux   arm   vertex-proxy     vertex-proxy-linux-arm32   scripts/start.sh scripts/vertex-proxy.service scripts/setup.sh ;;
    darwin/amd64)
      build darwin  amd64 vertex-proxy     vertex-proxy-darwin-amd64  scripts/start.sh scripts/setup.sh ;;
    darwin/arm64)
      build darwin  arm64 vertex-proxy     vertex-proxy-darwin-arm64  scripts/start.sh scripts/setup.sh ;;
    android/arm64)
      build android arm64 vertex-proxy     vertex-proxy-android-arm64 scripts/start.sh scripts/setup.sh ;;
    *)
      echo "错误：未知目标平台 $target" >&2
      exit 1 ;;
  esac
}

echo "目标平台: $TARGET_PLATFORMS"
for target in $TARGET_PLATFORMS; do
  build_platform "$target"
done

echo "完成。产物："
ls -1 "$OUT"/*.zip 2>/dev/null || echo "(无)"
