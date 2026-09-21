#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
bin="$tmp/prowl"
project="$tmp/project"

cd "$repo"
CGO_ENABLED=1 go build -tags sqlite_fts5 -o "$bin" ./cmd/prowl

mkdir -p "$project/.cursor"
printf 'package demo\n\nfunc Hello() string { return "hello" }\n' > "$project/main.go"
printf '# Demo\n' > "$project/README.md"

cd "$project"
"$bin" init --dry-run --json --integrations cursor,agents > "$tmp/plan.json"
python3 - "$tmp/plan.json" <<'PY'
import json, pathlib, sys
payload = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert payload['dry_run'] is True
# Selecting cursor,agents plans those two destinations plus the per-skill installs
# they imply, and nothing else. Asserting equality with {agents, cursor} silently
# rotted when skill actions joined the plan.
kinds = {a['integration'] for a in payload['plan']['actions']}
assert {'agents', 'cursor'} <= kinds, kinds
assert kinds <= {'agents', 'cursor', 'skill'}, kinds
PY
test ! -e .prowl

"$bin" init --no-input --json --integrations cursor,agents > "$tmp/init.json"
python3 - "$tmp/init.json" <<'PY'
import json, pathlib, sys
report = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert report['indexed']['Indexed'] == 2, report
assert report['integrations'] == ['agents', 'cursor'], report
PY
test -f .cursor/mcp.json
test -f AGENTS.md

"$bin" overview --format human | grep -q 'Project overview'
"$bin" overview --format json | python3 -m json.tool >/dev/null
"$bin" doctor --json | python3 -m json.tool >/dev/null

"$bin" init --remove-integrations --no-input --json --integrations cursor,agents > "$tmp/remove.json"
test ! -e AGENTS.md
python3 - <<'PY'
import json, pathlib
servers = json.loads(pathlib.Path('.cursor/mcp.json').read_text()).get('mcpServers', {})
# Removal must drop the active renamed key; the retired legacy name stays gone too.
assert 'prowl' not in servers, servers
assert 'prowl-agent' not in servers, servers
PY

echo 'onboarding smoke test passed'
