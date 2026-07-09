#!/bin/sh
# claude-says installer — downloads the latest macOS release binary, verifies its
# checksum, and installs it onto your PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/Sudhanshu069/claude-says/main/install.sh | sh
#
# Override the install dir with BINDIR, e.g. BINDIR="$HOME/.local/bin".
set -eu

REPO="Sudhanshu069/claude-says"
BINDIR="${BINDIR:-/usr/local/bin}"

fail() { echo "error: $*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || fail "claude-says is macOS-only (got $(uname -s))."
command -v curl >/dev/null 2>&1 || fail "curl is required."

case "$(uname -m)" in
  arm64 | aarch64) arch="arm64" ;;
  x86_64 | amd64) arch="amd64" ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

# Resolve the latest tag from the releases/latest redirect (no GitHub API rate limit).
tag="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest" | sed 's#.*/tag/##')"
[ -n "$tag" ] || fail "could not determine the latest release."
ver="${tag#v}"
asset="claude-says_${ver}_darwin_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading claude-says $tag ($arch)…"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || fail "download failed: $base/$asset"

# Verify SHA-256 against the release checksums when available.
if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  want="$(awk -v a="$asset" '$2 == a {print $1}' "$tmp/checksums.txt")"
  got="$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')"
  if [ -n "$want" ] && [ "$want" != "$got" ]; then
    fail "checksum mismatch (expected $want, got $got)"
  fi
  [ -n "$want" ] && echo "checksum verified."
fi

tar -xzf "$tmp/$asset" -C "$tmp" || fail "extract failed."
[ -f "$tmp/claude-says" ] || fail "archive did not contain the claude-says binary."
chmod +x "$tmp/claude-says"

echo "Installing to $BINDIR…"
if [ -w "$BINDIR" ] || [ "$(id -u)" = "0" ]; then
  mv "$tmp/claude-says" "$BINDIR/claude-says"
else
  sudo mv "$tmp/claude-says" "$BINDIR/claude-says"
fi

echo ""
"$BINDIR/claude-says" --version
echo ""
echo "Installed. Next:"
echo "  claude-says setup    # pick a voice + install the Claude Code Stop hook"
echo "  claude-says start    # start speaking"
