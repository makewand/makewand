#!/usr/bin/env bash
set -euo pipefail

REPO="${MAKEWAND_REPO:-bnumsn/makewand}"
VERSION="${MAKEWAND_VERSION:-latest}"
INSTALL_DIR="${MAKEWAND_INSTALL_DIR:-$HOME/.local/bin}"

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

need_cmd curl
need_cmd uname

detect_os() {
  case "$(uname -s)" in
    Linux) echo "linux" ;;
    Darwin) echo "darwin" ;;
    *)
      echo "unsupported OS: $(uname -s)" >&2
      exit 1
      ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

resolve_tag() {
  if [ "$VERSION" != "latest" ]; then
    echo "$VERSION"
    return
  fi
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' \
    | head -n 1
}

TAG="$(resolve_tag)"
if [ -z "$TAG" ]; then
  echo "failed to resolve release tag for ${REPO}" >&2
  exit 1
fi

OS="$(detect_os)"
ARCH="$(detect_arch)"

EXT="tar.gz"
if [ "$OS" = "windows" ]; then
  EXT="zip"
fi

ASSET="makewand_${TAG}_${OS}_${ARCH}.${EXT}"
URL="https://github.com/${REPO}/releases/download/${TAG}/${ASSET}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading ${URL}"
curl -fL "$URL" -o "${TMP_DIR}/${ASSET}"

mkdir -p "$INSTALL_DIR"

if [ "$EXT" = "zip" ]; then
  need_cmd unzip
  unzip -q "${TMP_DIR}/${ASSET}" -d "${TMP_DIR}"
else
  tar -xzf "${TMP_DIR}/${ASSET}" -C "${TMP_DIR}"
fi

BIN_PATH="$(find "${TMP_DIR}" -type f -name 'makewand' | head -n 1 || true)"
if [ -z "$BIN_PATH" ]; then
  BIN_PATH="$(find "${TMP_DIR}" -type f -name 'makewand.exe' | head -n 1 || true)"
fi
if [ -z "$BIN_PATH" ]; then
  echo "failed to locate makewand binary in archive" >&2
  exit 1
fi

install -m 0755 "$BIN_PATH" "${INSTALL_DIR}/makewand"

echo "Installed makewand to ${INSTALL_DIR}/makewand"
echo "Run: makewand --version"
