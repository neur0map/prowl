#!/bin/sh
# Install the latest stable Prowl build on Linux or macOS. Set PROWL_RELEASE_BASE
# to .../releases/download/preview for the unreviewed preview channel.
#   curl -fsSL https://raw.githubusercontent.com/neur0map/prowl/main/install.sh | sh
set -eu

REPO="neur0map/prowl"
BASE="${PROWL_RELEASE_BASE:-https://github.com/$REPO/releases/download/stable}"
DEST="${PROWL_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported operating system: $(uname -s). Use install.ps1 on Windows." >&2; exit 1 ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m). Build from source instead." >&2; exit 1 ;;
esac

BIN="prowl-${os}-${arch}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

echo "Downloading $BIN ..."
curl -fsSL -o "$tmp/$BIN" "$BASE/$BIN"
curl -fsSL -o "$tmp/$BIN.sha256" "$BASE/$BIN.sha256"

want="$(awk '{print $1}' "$tmp/$BIN.sha256")"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$BIN" | awk '{print $1}')"
else
  got="$(shasum -a 256 "$tmp/$BIN" | awk '{print $1}')"
fi
[ "$want" = "$got" ] || { echo "Checksum mismatch; aborting." >&2; exit 1; }

mkdir -p "$DEST"
install -m 0755 "$tmp/$BIN" "$DEST/prowl"

# The transitional prowl-agent command is retired. Now that the new prowl binary
# is in place, remove the legacy sibling from this installer's own destination
# only -- a prowl-agent found elsewhere on PATH is never touched. Skipping a
# directory of that name and using rm -f keep a repeat install idempotent.
legacy="$DEST/prowl-agent"
[ -d "$legacy" ] || rm -f "$legacy"
printf '\n  Prowl installed to %s/prowl\n  Next: cd <project> && prowl init\n\n' "$DEST"

case ":$PATH:" in
  *":$DEST:"*) ;;
  *) printf '  Note: add %s to PATH, for example:\n        export PATH="%s:$PATH"\n\n' "$DEST" "$DEST" ;;
esac
