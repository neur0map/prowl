#!/bin/sh
# Exercise Prowl against a project mounted read-only. All derived state lives in
# a temporary overlay; the supplied project and its existing .prowl directory
# remain untouched.
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: $0 <project-root>" >&2
    exit 2
fi

project=$(CDPATH='' cd -- "$1" && pwd)
repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

[ -d "$project" ] || { echo "project is not a directory: $project" >&2; exit 1; }
command -v bwrap >/dev/null 2>&1 || { echo "bwrap is required" >&2; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

tmp=$(mktemp -d)
cleanup() {
    # Overlayfs creates a mode-000 work directory. Restore access before
    # removing the harness state.
    chmod -R u+rwx "$tmp" 2>/dev/null || true
    rm -rf "$tmp"
}
trap cleanup EXIT HUP INT TERM

binary="$tmp/prowl"
upper="$tmp/upper"
work="$tmp/work"
mkdir -p "$upper" "$work"

(
    cd "$repo"
    CGO_ENABLED=1 go build -tags sqlite_fts5 -o "$binary" ./cmd/prowl
)

# shellcheck disable=SC2016
bwrap \
    --die-with-parent \
    --unshare-net \
    --ro-bind / / \
    --tmpfs /tmp \
    --ro-bind "$binary" /tmp/prowl \
    --dir /tmp/project \
    --overlay-src "$project" \
    --overlay "$upper" "$work" /tmp/project \
    --dev /dev \
    --proc /proc \
    --clearenv \
    --setenv HOME /tmp/home \
    --setenv PATH /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    --setenv XDG_CACHE_HOME /tmp/cache \
    --setenv XDG_CONFIG_HOME /tmp/config \
    --setenv XDG_STATE_HOME /tmp/state \
    --chdir /tmp/project \
    /bin/sh -ceu '
        "$1" init --no-input --json --integrations "" > /tmp/init-first.json
        "$1" init --no-input --json --integrations "" > /tmp/init-second.json
        python3 -m json.tool /tmp/init-second.json >/dev/null
        "$1" overview --format json | python3 -m json.tool >/dev/null
        "$1" status --json | python3 -m json.tool >/dev/null
    ' sh /tmp/prowl

echo "read-only corpus smoke test passed: $project"
