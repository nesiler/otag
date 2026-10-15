#!/bin/sh
# otag installer: curl -fsSL https://raw.githubusercontent.com/nesiler/otag/main/install.sh | sh
set -eu

REPO="nesiler/otag"
INSTALL_DIR="${OTAG_INSTALL_DIR:-$HOME/.local/bin}"
# Test hook (not a user option): where release assets are downloaded from.
DOWNLOAD_BASE="${OTAG_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}"

usage() {
  cat <<EOF
Install otag (macOS only).

Usage: install.sh [--help]

Environment:
  OTAG_VERSION      Version to install, e.g. v0.1.0 (default: latest release)
  OTAG_INSTALL_DIR  Install directory (default: \$HOME/.local/bin)
EOF
}

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) usage >&2; exit 2 ;;
esac

[ "$(uname -s)" = "Darwin" ] || die "otag supports macOS only (it relies on launchd)."

case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64) arch=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required."
command -v shasum >/dev/null 2>&1 || die "shasum is required."

version="${OTAG_VERSION:-}"
if [ -z "$version" ]; then
  final_url="$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")" ||
    die "could not resolve the latest release."
  version="${final_url##*/}"
fi
case "$version" in
  v*) ;;
  *) version="v$version" ;;
esac
num="${version#v}"

archive="otag_${num}_darwin_${arch}.tar.gz"
base="$DOWNLOAD_BASE/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'Downloading otag %s (darwin/%s)...\n' "$version" "$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || die "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || die "download failed: $base/checksums.txt"

expected="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$expected" ] || die "no checksum for $archive in checksums.txt."
actual="$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')"
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive."
echo "Checksum verified."

mkdir -p "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" otag || die "archive does not contain the otag binary."

mkdir -p "$INSTALL_DIR"
cp "$tmp/x/otag" "$INSTALL_DIR/otag.new"
chmod 755 "$INSTALL_DIR/otag.new"
mv -f "$INSTALL_DIR/otag.new" "$INSTALL_DIR/otag"
# Unsigned binary: drop the quarantine flag if one was set.
xattr -d com.apple.quarantine "$INSTALL_DIR/otag" 2>/dev/null || true
printf 'Installed %s/otag\n' "$INSTALL_DIR"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) printf "\nwarning: %s is not on your PATH. Add this to your shell profile:\n  export PATH=\"%s:\$PATH\"\n" "$INSTALL_DIR" "$INSTALL_DIR" ;;
esac

for dep in tmux git; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    printf '\nwarning: %s is required at runtime but was not found. Install it with:\n  brew install %s\n' "$dep" "$dep"
  fi
done

cat <<EOF

Next steps:
  otag                  # first run: the setup wizard (agents, a project, the daemon)
  otag doctor           # check your environment; every problem comes with its fix
  otag tui              # open the terminal UI (later, plain otag does too)
EOF
