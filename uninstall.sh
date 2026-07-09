#!/bin/sh
# claude-says uninstaller: reverses setup (removes the Claude Code Stop hook and
# ~/.claude-says via the binary's own `uninstall` command), then removes the binary.
#
#   curl -fsSL https://raw.githubusercontent.com/Sudhanshu069/claude-says/main/uninstall.sh | sh
#
# Keep the binary with KEEP_BINARY=1; keep config with `claude-says uninstall --keep-config`.
set -eu

BINDIR="${BINDIR:-/usr/local/bin}"

# Locate the binary: PATH first, then BINDIR.
if command -v claude-says >/dev/null 2>&1; then
  bin="$(command -v claude-says)"
elif [ -x "${BINDIR}/claude-says" ]; then
  bin="${BINDIR}/claude-says"
else
  bin=""
fi

# Reverse setup (remove the Stop hook + ~/.claude-says).
if [ -n "${bin}" ]; then
  "${bin}" uninstall || true
else
  echo "claude-says binary not found; skipping hook/config cleanup."
  echo "If a hook remains, remove the claude-says entry from ~/.claude/settings.json,"
  echo "and delete ~/.claude-says."
fi

# Remove the binary.
if [ "${KEEP_BINARY:-0}" != "1" ] && [ -n "${bin}" ] && [ -f "${bin}" ]; then
  dir="$(dirname "${bin}")"
  if [ -w "${dir}" ]; then rm -f "${bin}"; else sudo rm -f "${bin}"; fi
  echo "Removed ${bin}"
fi
echo "Done."
