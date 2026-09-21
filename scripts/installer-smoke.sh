#!/bin/sh
# Exercise install.sh end to end against a local file:// release, proving the
# renamed prowl binary lands and the retired prowl-agent sibling is cleaned from
# the installer's own destination only -- a prowl-agent elsewhere on PATH is
# never touched, and a repeat install stays idempotent.
set -eu

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
installer="$repo/install.sh"
[ -r "$installer" ] || { echo "install.sh not found at $installer" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "installer smoke unsupported on $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "installer smoke unsupported on $(uname -m)" >&2; exit 1 ;;
esac
bin="prowl-${os}-${arch}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
release="$tmp/release"
dest="$tmp/bin"
elsewhere="$tmp/other"
mkdir -p "$release" "$dest" "$elsewhere"

# A stand-in release artifact plus its checksum, served over file://.
printf '#!/bin/sh\necho prowl-stub\n' > "$release/$bin"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$release/$bin" | awk '{print $1}' > "$release/$bin.sha256"
else
  shasum -a 256 "$release/$bin" | awk '{print $1}' > "$release/$bin.sha256"
fi

# A stale legacy sibling in the install destination (must be removed) and an
# unrelated prowl-agent elsewhere on PATH (must survive).
printf 'stale-legacy\n' > "$dest/prowl-agent"
chmod +x "$dest/prowl-agent"
printf 'unrelated\n' > "$elsewhere/prowl-agent"
chmod +x "$elsewhere/prowl-agent"

run_install() {
  PROWL_RELEASE_BASE="file://$release" PROWL_INSTALL_DIR="$dest" \
    sh "$installer" >/dev/null
}

# First install: renamed binary lands, stale legacy sibling is gone, and the
# prowl-agent outside the destination is left alone.
run_install
[ -x "$dest/prowl" ] || { echo "install did not place prowl in $dest" >&2; exit 1; }
[ ! -e "$dest/prowl-agent" ] || { echo "stale legacy prowl-agent survived install" >&2; exit 1; }
[ -e "$elsewhere/prowl-agent" ] || { echo "installer removed a prowl-agent outside its destination" >&2; exit 1; }

# Second install with no legacy present: idempotent, no error, prowl still there.
run_install
[ -x "$dest/prowl" ] || { echo "second install lost prowl" >&2; exit 1; }
[ ! -e "$dest/prowl-agent" ] || { echo "second install resurrected prowl-agent" >&2; exit 1; }

echo "installer smoke test passed"
