#!/usr/bin/env bash
set -euo pipefail

REPO="${MAKEWAND_REPO:-makewand/makewand}"
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
CHECKSUMS_URL="https://github.com/${REPO}/releases/download/${TAG}/checksums.txt"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading ${URL}"
curl -fL "$URL" -o "${TMP_DIR}/${ASSET}"

echo "Downloading ${CHECKSUMS_URL}"
curl -fL "${CHECKSUMS_URL}" -o "${TMP_DIR}/checksums.txt"

verify_asset_checksum() {
  local expected actual
  expected="$(awk -v f="${ASSET}" '$2 == f {print $1}' "${TMP_DIR}/checksums.txt" | head -n 1)"
  if [ -z "${expected}" ]; then
    echo "checksum entry not found for ${ASSET}" >&2
    exit 1
  fi

  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
  elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "${TMP_DIR}/${ASSET}" | awk '{print $1}')"
  else
    echo "missing required command: sha256sum (or shasum)" >&2
    exit 1
  fi

  if [ "${actual}" != "${expected}" ]; then
    echo "checksum verification failed for ${ASSET}" >&2
    echo "expected: ${expected}" >&2
    echo "actual:   ${actual}" >&2
    exit 1
  fi
  echo "Checksum verified: ${ASSET}"
}

verify_asset_checksum

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
