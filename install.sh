#!/bin/sh
# inferenced installer: download the latest release for this OS/arch, verify its
# sha256 against checksums.txt, and install the binary to ~/.local/bin.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Fraser-Isbester/inferenced/main/install.sh | sh
#
# Env overrides:
#   INFERENCED_BIN   install directory (default: $HOME/.local/bin)
#   INFERENCED_VERSION  release tag to install (default: latest)
set -eu

REPO="Fraser-Isbester/inferenced"
BIN_DIR="${INFERENCED_BIN:-$HOME/.local/bin}"

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

need uname
need curl
need tar

# Detect OS.
os="$(uname -s)"
case "$os" in
	Darwin) os="darwin" ;;
	Linux) os="linux" ;;
	*) fail "unsupported operating system: $os (supported: Darwin, Linux)" ;;
esac

# Detect architecture, normalizing to goreleaser's naming.
arch="$(uname -m)"
case "$arch" in
	arm64 | aarch64) arch="arm64" ;;
	x86_64 | amd64) arch="amd64" ;;
	*) fail "unsupported architecture: $arch (supported: arm64, amd64)" ;;
esac

# Resolve the release tag. Prefer an explicit override; otherwise ask the GitHub
# API for the latest release.
tag="${INFERENCED_VERSION:-}"
if [ -z "$tag" ]; then
	api="https://api.github.com/repos/${REPO}/releases/latest"
	tag="$(curl -fsSL "$api" | grep '"tag_name"' | head -n 1 | cut -d '"' -f 4)"
fi
[ -n "$tag" ] || fail "could not resolve the latest release tag"

# goreleaser strips the leading "v" from the version in artifact names.
version="${tag#v}"

archive="inferenced_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${tag}"

# Work in a self-cleaning temp directory.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "Downloading ${archive} (${tag})..."
curl -fsSL "${base_url}/${archive}" -o "${tmp}/${archive}" ||
	fail "download failed for ${archive}; is ${os}/${arch} published for ${tag}?"
curl -fsSL "${base_url}/checksums.txt" -o "${tmp}/checksums.txt" ||
	fail "download failed for checksums.txt"

# Verify the sha256 checksum. Prefer sha256sum, fall back to shasum (macOS).
echo "Verifying checksum..."
expected="$(grep " ${archive}\$" "${tmp}/checksums.txt" | cut -d ' ' -f 1)"
[ -n "$expected" ] || fail "no checksum entry for ${archive}"

if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${tmp}/${archive}" | cut -d ' ' -f 1)"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "${tmp}/${archive}" | cut -d ' ' -f 1)"
else
	fail "no sha256 tool found (need sha256sum or shasum)"
fi

[ "$expected" = "$actual" ] ||
	fail "checksum mismatch for ${archive}: expected ${expected}, got ${actual}"

# Extract and install.
echo "Installing to ${BIN_DIR}..."
tar -xzf "${tmp}/${archive}" -C "$tmp"
[ -f "${tmp}/inferenced" ] || fail "archive did not contain the inferenced binary"

mkdir -p "$BIN_DIR"
install -m 0755 "${tmp}/inferenced" "${BIN_DIR}/inferenced" 2>/dev/null ||
	{ cp "${tmp}/inferenced" "${BIN_DIR}/inferenced" && chmod +x "${BIN_DIR}/inferenced"; }

echo "Installed inferenced ${tag} to ${BIN_DIR}/inferenced"

# PATH hint.
case ":${PATH}:" in
	*":${BIN_DIR}:"*) ;;
	*) echo "Note: ${BIN_DIR} is not on your PATH. Add it, e.g.:" &&
		echo "  export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac

echo "Next: run 'inferenced install' to set up the local gateway service."
