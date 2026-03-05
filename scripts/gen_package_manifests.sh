#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 3 ]]; then
  echo "usage: $0 <tag> <owner/repo> <checksums-path> [out-dir]" >&2
  exit 1
fi

TAG="$1"
REPO="$2"
CHECKSUMS_PATH="$3"
OUT_DIR="${4:-dist}"
VERSION="${TAG#v}"

if [[ "${TAG}" == "${VERSION}" ]]; then
  echo "tag must start with 'v' (example: v0.1.7)" >&2
  exit 1
fi

if [[ ! -f "${CHECKSUMS_PATH}" ]]; then
  echo "checksums file not found: ${CHECKSUMS_PATH}" >&2
  exit 1
fi

asset_name() {
  local os="$1"
  local arch="$2"
  local ext="$3"
  echo "makewand_${TAG}_${os}_${arch}.${ext}"
}

checksum_for() {
  local file="$1"
  awk -v f="${file}" '$2 == f {print $1}' "${CHECKSUMS_PATH}" | head -n 1
}

require_checksum() {
  local file="$1"
  local sum
  sum="$(checksum_for "${file}")"
  if [[ -z "${sum}" ]]; then
    echo "missing checksum entry for ${file}" >&2
    exit 1
  fi
  echo "${sum}"
}

LINUX_AMD64="$(asset_name linux amd64 tar.gz)"
LINUX_ARM64="$(asset_name linux arm64 tar.gz)"
DARWIN_AMD64="$(asset_name darwin amd64 tar.gz)"
DARWIN_ARM64="$(asset_name darwin arm64 tar.gz)"
WINDOWS_AMD64="$(asset_name windows amd64 zip)"

SUM_LINUX_AMD64="$(require_checksum "${LINUX_AMD64}")"
SUM_LINUX_ARM64="$(require_checksum "${LINUX_ARM64}")"
SUM_DARWIN_AMD64="$(require_checksum "${DARWIN_AMD64}")"
SUM_DARWIN_ARM64="$(require_checksum "${DARWIN_ARM64}")"
SUM_WINDOWS_AMD64="$(require_checksum "${WINDOWS_AMD64}")"

RELEASE_BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"
HOMEBREW_OUT="${OUT_DIR}/homebrew/Formula/makewand.rb"
SCOOP_OUT="${OUT_DIR}/scoop/makewand.json"

mkdir -p "$(dirname "${HOMEBREW_OUT}")" "$(dirname "${SCOOP_OUT}")"

cat >"${HOMEBREW_OUT}" <<EOF
class Makewand < Formula
  desc "AI coding assistant CLI"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "${RELEASE_BASE_URL}/${DARWIN_ARM64}"
      sha256 "${SUM_DARWIN_ARM64}"
    else
      url "${RELEASE_BASE_URL}/${DARWIN_AMD64}"
      sha256 "${SUM_DARWIN_AMD64}"
    end
  end

  on_linux do
    if Hardware::CPU.arm?
      url "${RELEASE_BASE_URL}/${LINUX_ARM64}"
      sha256 "${SUM_LINUX_ARM64}"
    else
      url "${RELEASE_BASE_URL}/${LINUX_AMD64}"
      sha256 "${SUM_LINUX_AMD64}"
    end
  end

  def install
    bin.install Dir["*/makewand"].first
  end

  test do
    assert_match "makewand version", shell_output("#{bin}/makewand --version")
  end
end
EOF

cat >"${SCOOP_OUT}" <<EOF
{
  "version": "${VERSION}",
  "description": "AI coding assistant CLI",
  "homepage": "https://github.com/${REPO}",
  "license": "MIT",
  "architecture": {
    "64bit": {
      "url": "${RELEASE_BASE_URL}/${WINDOWS_AMD64}",
      "hash": "${SUM_WINDOWS_AMD64}"
    }
  },
  "extract_dir": "makewand_${TAG}_windows_amd64",
  "bin": "makewand.exe",
  "checkver": "github",
  "autoupdate": {
    "architecture": {
      "64bit": {
        "url": "https://github.com/${REPO}/releases/download/v\$version/makewand_v\$version_windows_amd64.zip"
      }
    },
    "extract_dir": "makewand_v\$version_windows_amd64"
  }
}
EOF

echo "Generated:"
echo "  - ${HOMEBREW_OUT}"
echo "  - ${SCOOP_OUT}"
