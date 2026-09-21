# Contributing to Prowl

Prowl is a local-first Go application with CGO-backed SQLite extensions. Keep changes small, cited, and compatible with existing CLI and MCP clients.

## Prerequisites

- Go version from `go.mod`
- A C compiler
- SQLite development headers (`libsqlite3-dev` on Debian/Ubuntu, `brew install sqlite3` on macOS)
- Node.js only for the web workbench and Obsidian plugin packages

## Build and test

```sh
CGO_ENABLED=1 go test -tags sqlite_fts5 ./...
CGO_ENABLED=1 go vet -tags sqlite_fts5 ./...
CGO_ENABLED=1 go build -tags sqlite_fts5 ./cmd/prowl
bash scripts/onboarding-smoke.sh
```

Plain `go test ./...` is not a valid project test command: the SQLite driver must be built with the `sqlite_fts5` tag.

For workbench changes, install the exact lockfile and pinned Chromium build, then
run the unit, type, production-build, real-binary, accessibility, and browser
checks:

```sh
cd web
npm ci
npx playwright install chromium
npm audit --audit-level=low
npm run check
cd ..
git diff --exit-code -- web/dist
```

`web/dist` is committed so ordinary Go builds do not require Node.js. Rebuild it
with `npm run build`; never edit generated bundle files by hand.

## Change discipline

1. Add or update a test that demonstrates the behavior or bug.
2. Run it and confirm the expected failure.
3. Make the smallest implementation change that passes.
4. Run the full tagged suite and `go vet`.
5. Keep migrations reversible and back up user-owned state before destructive schema changes.
6. Preserve existing machine-readable output and MCP contracts unless a versioned migration is included.
7. Do not add AI/co-author attribution to commits.

## Product boundaries

- Structural indexing must work without Ollama, cloud credentials, or a daemon.
- Network-facing services bind to loopback by default and require authentication for state-changing operations.
- Integration setup writes only selected clients and only removes Prowl-owned entries.
- Generated setup guidance must not pollute retrieval.
- Optional domain behavior belongs in an explicit profile or capability pack.
