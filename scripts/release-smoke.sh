#!/bin/sh
# Smoke-test a locally built or downloaded release binary without touching its source tree.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <binary> <binary.sha256>" >&2
  exit 2
fi

binary=$(CDPATH='' cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")
checksum=$(CDPATH='' cd -- "$(dirname -- "$2")" && pwd)/$(basename -- "$2")

[ -x "$binary" ] || { echo "release binary is not executable: $binary" >&2; exit 1; }
[ -r "$checksum" ] || { echo "release checksum is not readable: $checksum" >&2; exit 1; }

want=$(awk '{print $1}' "$checksum")
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$binary" | awk '{print $1}')
else
  got=$(shasum -a 256 "$binary" | awk '{print $1}')
fi
[ "$want" = "$got" ] || { echo "release checksum mismatch" >&2; exit 1; }

tmp=$(mktemp -d)
export XDG_CACHE_HOME="$tmp/cache"
export XDG_CONFIG_HOME="$tmp/config"
export XDG_STATE_HOME="$tmp/state"
export HOME="$tmp/home"
mkdir -p "$HOME"
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
project="$tmp/project"
mkdir -p "$project/.cursor"
printf 'package demo\n\nfunc Hello() string { return "hello" }\n' > "$project/main.go"
printf '# Demo\n' > "$project/README.md"

cd "$project"
"$binary" init --dry-run --json --integrations cursor,agents > "$tmp/plan.json"
python3 - "$tmp/plan.json" <<'PY'
import json, pathlib, sys
payload = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert payload['dry_run'] is True
actions = payload['plan']['actions']
assert {a['integration'] for a in actions if a['integration'] != 'skill'} == {'agents', 'cursor'}, actions
assert any(a['integration'] == 'skill' for a in actions), actions
PY
test ! -e .prowl

"$binary" init --no-input --json --integrations cursor,agents > "$tmp/init.json"
python3 - "$tmp/init.json" <<'PY'
import json, pathlib, sys
report = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert report['indexed']['Indexed'] == 2, report
assert report['integrations'] == ['agents', 'cursor'], report
PY
test -f .cursor/mcp.json
test -f AGENTS.md
# Skills install under each selected integration's own dir; cursor was requested
# above, so the review skill lands in .cursor/skills (the agents integration
# writes AGENTS.md, and .agents/skills is the separate agent-skills integration).
review_skill=.cursor/skills/prowl-pr-review/SKILL.md
test -f "$review_skill" || { echo "standard integration install omitted prowl-pr-review" >&2; exit 1; }
grep -q '^name: prowl-pr-review$' "$review_skill" || { echo "installed review skill has invalid frontmatter" >&2; exit 1; }
grep -q 'prowl review check --review <id> --report <regular-file|->' "$review_skill" || {
  echo "installed review skill omitted the native coverage check contract" >&2
  exit 1
}

"$binary" overview --format json | python3 -m json.tool >/dev/null
"$binary" status --json | python3 -m json.tool >/dev/null
"$binary" update --help >/dev/null

# The agent-skill installer is preview-only when non-interactive: even with the
# Claude and OMP config roots present under an isolated HOME, a piped invocation
# must render a plan and write nothing -- no assets, no ownership manifest.
mkdir -p "$HOME/.claude" "$HOME/.omp/agent"
"$binary" skills </dev/null > "$tmp/skills.txt"
grep -q "nothing was written" "$tmp/skills.txt" || { echo "skills did not report a no-write preview" >&2; cat "$tmp/skills.txt" >&2; exit 1; }
test ! -e "$HOME/.claude/skills" || { echo "skills wrote Claude assets in preview mode" >&2; exit 1; }
test ! -e "$HOME/.omp/agent/skills" || { echo "skills wrote OMP assets in preview mode" >&2; exit 1; }
test ! -e "$XDG_STATE_HOME/prowl-agent/agent-assets.json" || { echo "skills committed the ownership manifest in preview mode" >&2; exit 1; }

"$binary" init --remove-integrations --no-input --json --integrations cursor,agents > "$tmp/remove.json"
test ! -e AGENTS.md
python3 - <<'PY'
import json, pathlib
servers = json.loads(pathlib.Path('.cursor/mcp.json').read_text()).get('mcpServers', {})
# Removal must drop the active renamed key; the retired legacy name stays gone too.
assert 'prowl' not in servers, servers
assert 'prowl-agent' not in servers, servers
PY

echo "release smoke test passed"
