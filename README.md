# Prowl Agent

**Your coding agent greps the repo and re-reads the same files on every turn to rebuild context it already threw away. Prowl indexes the project once and answers those questions in a single command, cited to `file:line`, for a fraction of the tokens.**

[![ci](https://github.com/neur0map/prowl-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/neur0map/prowl-agent/actions/workflows/ci.yml)
[![version](https://img.shields.io/github/v/release/neur0map/prowl-agent?label=version&color=89b4fa)](https://github.com/neur0map/prowl-agent/releases/latest)
[![platform](https://img.shields.io/badge/platform-Linux%20%7C%20macOS%20%7C%20Windows-555)](#install)

Prowl builds a small SQLite index of your project: the files, the symbols inside
them, and how they wire together. Your agent runs one command and gets a short,
exact, cited answer instead of a wall of grep hits. Answers come back in
[TOON](https://toonformat.dev), which models read for roughly 40% fewer tokens
than JSON. No server, no daemon, nothing leaves your machine.

That is the whole pitch. It is not a chat wrapper, an autonomous agent, or a
graph you stare at. It is the index your agent should have asked first.

```console
$ prowl-agent find NewGui            # exact hits with signatures, all cited
[4]{end_line,file,id,kind,line,name,signature}:
  266,pkg/gocui/gui.go,2709,function,212,NewGui,"func NewGui(opts NewGuiOpts) (*Gui, error)"
  799,pkg/gui/gui.go,4977,function,723,NewGui,"func NewGui( cmn *common.Common, configurer config.AppConfigurer, ... )"
  44,pkg/commands/oscommands/gui_io.go,2314,function,32,NewGuiIO,"func NewGuiIO( log *logrus.Entry, ... ) *guiIO"
  209,pkg/gocui/gui.go,2708,type,198,NewGuiOpts,"NewGuiOpts struct { OutputMode OutputMode ... }"

$ prowl-agent impact pkg/gui/gui.go  # who breaks if I touch this file
total: 7
direct: 2
by_subsystem[5]{count,subsystem}:
  2,pkg/cheatsheet
  2,pkg/integration
  ...
```

The grep version of the first question dumps a hit list, then the agent opens
each file to find the right `NewGui` and read its signature: kilobytes to tens of
kilobytes just to locate one symbol. Prowl answers in under a kilobyte and hands
the agent each signature inline, so it knows which one to call without opening a
file.

## Install

Linux and macOS (amd64 or arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/neur0map/prowl-agent/main/install.sh | sh
```

Windows amd64 from PowerShell:

```powershell
irm https://raw.githubusercontent.com/neur0map/prowl-agent/main/install.ps1 | iex
```

Windows may show "Windows protected your PC" or flag the `.exe`. It is an
unsigned Go binary, which antivirus tools often flag as a false positive; the
same code on Linux and macOS is fine. Verify the download against its `.sha256`
file, then unblock it (`Unblock-File .\prowl-agent.exe`) or click **More info,
Run anyway**. To avoid the warning, build from source (below).

Both installers pick the native artifact and verify its SHA-256 checksum. Linux
builds need a recent glibc. To build from source, install Go 1.26+, a C compiler,
and SQLite development headers (`libsqlite3-dev` on Debian/Ubuntu):

```sh
CGO_ENABLED=1 go build -tags sqlite_fts5 -o prowl-agent ./cmd/prowl-agent
```

Update in place with `prowl-agent update`. `prowl-agent status` tells you when a
new build is out, through a quick anonymous checksum check cached for a day.

## Demo

![prowl-agent demo](demo/prowl.gif)

A short terminal recording of `init`, `overview`, `bench`, and `graph`, rendered
from [`demo/prowl.tape`](demo/prowl.tape) with [VHS](https://github.com/charmbracelet/vhs).
Regenerate it with `vhs demo/prowl.tape`, or from the Actions tab via the `demo` workflow.

## Set up in one command

Run this once inside any project (a code repo, a dotfiles folder, `~/.config`):

```sh
prowl-agent init                                      # interactive client selection
prowl-agent init --dry-run --integrations auto        # exact preview, no writes
prowl-agent init --no-input --integrations cursor,vscode
```

`init` builds the index, previews the selected integrations, and writes only the
clients you choose. `--integrations auto` (the default) picks clients already
present in the project and always writes both the `AGENTS.md` guidance and a
client-agnostic `.mcp.json`, so any agent that reads the repo is told to prefer
Prowl *and* sees Prowl's tools in its own tool list (prose alone is unreliable
for coding agents with strong native grep/read priors). Use `none`, `all`, or a
comma-separated list. `--remove-integrations` removes only Prowl-owned entries
and leaves neighboring config alone. State lives in `.prowl/`, which it adds to
`.gitignore`.

Setup is transactional: a malformed client config aborts the run and restores any
file it already touched. The optional `AGENTS.md` guidance sits between markers,
so re-running setup refreshes only Prowl's block.

Project setup installs Prowl's `code-search` skill for detected harnesses so
structural repository questions start with the read-only CLI rather than a broad
tree scan. To install the same release-matched routing assets in your user-level
Claude and OMP configuration, run:

```sh
prowl-agent skills
```

`skills` first prints every destination and action, defaults to **No**, and
writes only after an interactive confirmation. A piped or otherwise
non-interactive invocation is always a read-only preview. Prowl records ownership
per asset, refuses to overwrite foreign files, and removes only files it owns.
The installed `code-search` skill handles natural repository questions;
Claude also gets `/prowl:search` as an explicit entry point. Verify the CLI,
assets, and whether either client needs a restart or reload with
`prowl-agent doctor --integrations`.

There is no server to keep running. Each CLI query re-indexes incrementally first
(only what changed, in tens of milliseconds), so the agent never reads stale data
and you never run a watcher by hand. The CLI is the canonical, lowest-overhead
agent path; MCP remains available as an optional compatibility transport.

## What your agent can ask

After `init`, the agent (or you) queries the index by running a command. This is
the lowest-overhead path: no server, and none of the tool-schema tokens an MCP
server adds to every request.

```sh
prowl-agent overview            # project map: docs to read, roles, entrypoints, clusters (start here)
prowl-agent brief <path>        # cited orientation for a subsystem: size, languages, guides, key files (warm-start a subagent)
prowl-agent find <name>         # locate a symbol (function, setting, keybind, component)
prowl-agent def <name>          # read one symbol's source (signature + body), cited and bounded, not the whole file
prowl-agent span <name>         # a symbol's current file+range plus a content digest, to spot drift before editing a stale range
prowl-agent outline <path>      # a file's structure: symbols, signatures, line ranges (no bodies) -- grasp a file without reading it
prowl-agent sketch <name|path>  # how a UI looks and behaves without a screenshot: QML, React (jsx/tsx), Go/lipgloss, or CSS
prowl-agent search <text>       # search by meaning or text; --smart rewrites+reranks, --compact lists files only
prowl-agent callers <path>      # what includes / imports / execs / binds to a file
prowl-agent callees <path>      # what a file includes / imports / execs / binds to
prowl-agent impact <path>       # blast radius: count, subsystems, direct importers (--all = full list)
prowl-agent relations <path>    # a file's symbols and include neighbors
prowl-agent entrypoints <path>  # root files from which this file is reachable
prowl-agent references <name>   # where a symbol is used: cited call sites (by name, or an id from find)
prowl-agent history <name>      # commits that touched a symbol (git log -L), newest first -- why the code is the way it is
prowl-agent clusters [name]     # subsystems (summaries); with a name, that subsystem's files
prowl-agent hotspots            # files ranked by graph centrality, plus largest and most complex
prowl-agent violations          # dangling refs, orphan scripts, hardcoded colors
prowl-agent doctor              # general health: cycles, fan risk, dangling refs, score
prowl-agent doctor --profile rice # add keybind, desktop command, color, orphan checks
prowl-agent tests <path>        # configs/keybinds that launch or reload a file
prowl-agent changed             # your git changes mapped to the files they could affect
prowl-agent wip                 # uncommitted work: touched files, TODO/FIXME markers, blast radius
prowl-agent graph               # interactive HTML dependency graph (self-contained, opens offline)
prowl-agent bench               # token efficiency: cited packets vs reading files vs whole repo
prowl-agent explore <path>      # index a repo you do not own, answer, and leave it untouched
prowl-agent context search <question> --mode compact --budget-tokens 1800 --json
prowl-agent capabilities search <query> # discover workflows without loading every schema
```

TTY output defaults to a human view; pipes and agent calls default to compact
TOON. Choose `--format human|toon|json|markdown` explicitly (`--json` is a
compatibility alias), or `--limit N` to cap results. Run from anywhere inside the
project; Prowl finds the index by walking up to `.prowl/`. Each answer stays
small: on a 2023-file Go repo, `overview` is about 1 KB and a typical `impact`
answer is a dozen lines, not the few hundred dependent rows the raw graph prints.

## Experimental native change review

The `review` command group is **EXPERIMENTAL**. It deterministically captures
and partitions a change for a host coding agent; Prowl itself does not perform
semantic review.

```sh
prowl-agent review plan                         # current workspace
prowl-agent review plan --base main --head HEAD # branch or pull-request range
prowl-agent review plan --commit abc123         # one non-merge commit
```

`review plan` selects `direct` or `structured` mode. The mandatory boundary is
exactly `raw_additions + raw_deletions > 300`: 300 lines remains direct, while
301 requires structured review. `--structured` opts into structured mode below
that boundary. Direct mode provides focused change context without a receipt
matrix; structured mode provides ordered cohorts, bounded units, required
cross-cutting audits, and an exact receipt contract.

The plan prints the next command for every unit. Fetch one territory at a time;
the owned patch is mandatory, while surrounding cited context is bounded:

```sh
prowl-agent review unit <review-id>/<unit-id>
prowl-agent review unit <review-id>/<unit-id> --budget-tokens 1200 --budget-bytes 8192
```

Plan, unit, and check output use the normal human, TOON, JSON, or Markdown
formats. The report input boundary is canonical `review.report.v1` JSON in a
regular file or on standard input:

```sh
prowl-agent review check --review <review-id> --report review-results.json
prowl-agent review check --review <review-id> --report -
```

The checker validates plan identity, evidence locations, workspace freshness,
recommendation consistency, plus complete unit and audit receipts in structured
mode. It rejects incomplete or stale reports with a non-zero exit; neither
state can approve a change. It validates this deterministic boundary, not the
semantic truth of a finding.

The commands are available for explicit use, but mandatory generated routing
and the GitHub Action integration are **not enabled** pending the frozen
held-out gates. See [the native review design and protocol](docs/REVIEW.md).

## Resume where you left off

`prowl-agent wip` answers "what was I in the middle of?" so a fresh session does
not have to re-read the tree to find out. It lists the files you have changed but
not committed (staged, modified, untracked), the unfinished-work markers inside
them (TODO, FIXME, HACK, XXX, BUG, WIP, OPTIMIZE, or your own via `--markers`),
and the blast radius of each indexed file. Same tool over MCP as `investigate_wip`.

## Keep durable project knowledge

Accepted concepts, decisions, claims, and playbooks live as portable OKF
Markdown, not inside the disposable SQLite index:

```bash
prowl-agent knowledge init
prowl-agent knowledge propose --file candidate.md --target decisions/storage.md
prowl-agent knowledge accept <proposal-id>
prowl-agent knowledge list
prowl-agent knowledge lint
prowl-agent knowledge lint --repair   # re-point anchors whose code moved
prowl-agent knowledge export ./knowledge-export
```

Every proposal shows a deterministic diff before acceptance. Source anchors keep
each claim tied to real code, and they distinguish code that *moved* from code
that *changed*: a line inserted above an anchored region reports `moved_anchor`
with the range those lines occupy now, and `--repair` re-points it, so notes do
not decay into stale warnings during ordinary refactoring. `stale_anchor` is
reserved for the anchored lines actually changing, which is the case a human
should look at. Anchor to a `symbol` (function, class, or component) to follow it
when lines move; a renamed symbol whose body is untouched is still recovered by
content. Unknown OKF v0.1 fields and future concept types round-trip without loss.

See [Durable knowledge and OKF](docs/KNOWLEDGE.md) for the storage contract,
review lifecycle, lint codes, and migration safeguards.

## Search external documentation

Point prowl at a library's docs once and query them offline, cited and token-
bounded, the same way you query code:

```bash
prowl-agent docs add https://docs.example.com   # crawl a docs site to Markdown
prowl-agent docs add ./vendor/docs --local      # or ingest a local Markdown tree
prowl-agent docs list
prowl-agent docs search "how do I configure retries"
```

Crawls are bounded and polite (depth, page cap, rate limit, robots.txt), and
pages are stored in a shared per-machine corpus, so a library's docs are crawled
once and reused across projects. Retrieval needs no model. Crawled pages are
untrusted, so any carrying prompt-injection directives are quarantined out of the
searchable corpus. Agents get the same over MCP through `search_docs`. When a site
publishes an `llms.txt` or `llms-full.txt`, `docs add` uses it directly (one fetch
of the whole docs, no crawl); pass `--no-llms` to force a plain crawl.

## One index, three ways to use it

The same `.prowl/index.db` is available three ways, with the CLI as the canonical
agent path:

- **Shell commands (recommended).** Any agent that can run a command can use
  Prowl. Nothing needs to stay running, and no MCP tool schemas consume context
  on every request. The installed `code-search` skill and Claude's
  `/prowl:search` command route both natural and explicit structural questions
  here.
- **MCP server (optional compatibility).** Select the standard `.mcp.json`,
  Cursor, VS Code, Oh My Pi, Factory droid, or OpenCode integration during
  project setup when a client requires typed tools. MCP Resources and Prompts
  remain additive on every surface. Use `prowl-agent serve --mcp-surface core`
  for eleven intent-oriented tools, or `--mcp-surface all` during migration.
  Point any other MCP client at `prowl-agent serve`.
- **Editor language server.** `prowl-agent lsp` gives a human go-to-definition,
  find-references, hover (with use counts), document and workspace symbols, code
  lens, completion, and inline `doctor` diagnostics. Neovim attaches it
  automatically; Helix and VS Code notes are in `.prowl/editor/SETUP.md`.

```json
{
  "mcpServers": {
    "prowl-agent": { "type": "stdio", "command": "prowl-agent", "args": ["serve"] }
  }
}
```

See [Context packets and MCP v2](docs/CONTEXT.md) for packet fields, resource
URIs, prompts, compatibility modes, privacy-safe traces, capability discovery,
and the retrieval evaluation.

## It understands how code connects

Locating a symbol is the easy half. The harder, more useful half is the graph:
what imports what, what a change ripples into, which files form a subsystem.
Prowl resolves real edges, not text matches:

- **Code imports.** Go package imports, TypeScript/JavaScript relative imports,
  Rust `mod` and `crate::` imports, Python absolute and relative imports, C/C++
  `#include`, Java and Kotlin `import` class paths (which resolve to each other in
  a mixed JVM project, and fold a member or nested-type import to its enclosing
  class file), Ruby `require_relative`, C# `using` namespaces, PHP `use Ns\Class`
  imports (resolved to the file declaring that class), Dart `package:` and
  relative imports (resolved to a workspace package's `lib/`), and Elixir
  `alias`/`import`/`use` (resolved to the file declaring that module).
- **Monorepos and path aliases.** A bare import of a first-party workspace package
  (`@scope/pkg` or `pkg/subpath`) resolves to that package's source, and a tsconfig
  path alias (`@/components/Button` with `"paths": {"@/*": ["src/*"]}`) resolves to
  the real file, scoped to the nearest `tsconfig.json` so a monorepo's per-package
  aliases stay correct. So `callers`, `impact`, and `clusters` work across a
  pnpm/turbo/Next.js project, not just within one package. The walk honors
  `.gitignore` negation, so a repo that ignores a tree but keeps its source
  (`packages/*/*/` then `!packages/*/src/`) is still indexed.
- **Configs.** Include trees (`source=`, `@import`, `require()`), exec and keybind
  chains (`exec-once`, `bind = ... exec script`), and shared colors, fonts, paths,
  and variables across files.

This is tested against real, popular repositories. A few results:

| repo | language | what the graph now sees |
|---|---|---|
| [lazygit](https://github.com/jesseduffield/lazygit) | Go, 2023 files | `impact`, `callers`, `clusters` across every package |
| [tRPC](https://github.com/trpc/trpc) | TS monorepo | 878 cross-package imports resolved; `impact @trpc/server` went 0 to 345 dependents |
| [zod](https://github.com/colinhacks/zod) | TS subpaths | `zod/v4/core` resolves to `packages/zod/src/v4/core/index.ts` |
| [Laravel](https://github.com/laravel/framework) | PHP, 2955 files | 8237 `use` imports resolved across components; `impact Str.php` reaches 1527 dependents |
| [OkHttp](https://github.com/square/okhttp) | Kotlin + Java | 1944 imports resolved across Kotlin-Multiplatform source sets, including cross-language and companion-member imports |
| [LocalSend](https://github.com/localsend/localsend) | Dart/Flutter | 977 `package:` imports resolved across a multi-package workspace; `impact` on a shared DTO reaches 73 files |
| [shadcn/ui](https://github.com/shadcn-ui/ui) | TS, 8869 files | 8382 `@/` tsconfig-alias imports resolved across many per-package configs |

Import resolution is strong for code and configs, and now models QML coupling
too: a component used by type name (`Button { }`) resolves to its `.qml` file,
and singleton or type member references (`Config.spacing`, `Theme.accent`)
resolve to the file that defines them. On a 1,413-file Quickshell repo this took
`impact` on the `Config.qml` singleton from 0 to 978 dependents. External and
standard-library imports stay informational. More languages are on the way.

## What it costs to run

`prowl-agent status` prints a card with what is indexed and, once your agent has
asked a few questions, how many tokens it saved. The number is grounded per
answer: for each query prowl served, it compares the bytes it returned against the
combined size of the files that answer pointed at (what an agent would otherwise
have read), then keeps about 70% of that as a deliberately under-counted estimate.
It tracks every project you have initialized and shows a combined total.

Run it in your terminal for the full colored card; pipe it for plain text, or add
`--json` for the raw numbers. Want to check the math on your own repos? See
[Measuring token usage](docs/TOKENS.md).

A rough idea of the gap, from a small test on real dotfile repos (not a benchmark
suite): indexed and asked the same question, `find` returned about 3 KB of cited
results in a couple of milliseconds, while opening every file ripgrep matched ran
to a few megabytes. A few hundred tokens versus hundreds of thousands, just to
locate something, before the agent reads anything. Your files, your question, and
your editor move these numbers, so measure on your own setup.

## Your code stays on your machine

Prowl indexes only what your project tracks (it honors `.gitignore`) and keeps its
own state in a local `.prowl/` folder, which it adds to `.gitignore`. The update
check is an anonymous read of public commit data and sends nothing about you. No
daemon, no network service.

Gitignoring `.prowl/` does not hide your code from the agent: the agent reads your
real files, and `.prowl/` only holds the rebuildable index. Because prowl indexes
the same files git tracks, it never points the agent at a path it was told to
ignore.

### Committed credentials are masked before they are stored

Prowl indexes whatever a repository contains, credentials committed in source
included, and every retrieval path feeds stored text into an agent's context. So
masking happens at storage time, not on the way out: the on-disk index -- chunk
text, the full-text index, and the vectors -- never holds a cleartext secret.
Five sinks are masked: chunk text, symbol signatures, doc comments, resource
values, and raw dependency-edge text. The identifier survives and only the value
is destroyed, so `search stripe token` still finds the line while the key itself
reads `[redacted]`. `prowl-agent doctor` reports which files had values masked, so
a committed secret surfaces as something to rotate rather than being quietly
swallowed.

What is masked is deliberately limited to shapes that can be recognized without
guessing: vendor-prefixed provider keys, AWS key ids, Google keys, JWTs, the
password in a URL's userinfo, and PEM private key bodies. A homegrown secret with
no vendor prefix -- a random-looking value assigned to a secret-named variable --
is **not** masked, because no entropy heuristic separates it from ordinary code
reliably, and masking is destructive. Treat this as damage control for
credentials that should not be in the repository, not as a reason to commit them.

## Search by meaning, built in

`prowl-agent search` matches on meaning, not just words. Ask "how do I refresh
the widget" and it finds `reloadPanel()` even though they share no tokens. This
works in every repo with nothing to set up: prowl ships a small code-trained
embedding model (potion-code-16M, a static model that runs as a plain vector
lookup, ~60 MB) inside the binary and runs it in-process. No download, no daemon,
no GPU, no API key, and nothing leaves your machine. The first search in a
project embeds its files once (a few seconds); after that answers are cached and
fast. Embeddings live in `sqlite-vec` and are fused with full-text search, so you
get files that mean the same thing even when they share no words (for example,
"music spectrum" finds an `AudioVisualizer`).

Doc comments are indexed as their own field, so a file whose docstring answers the
question surfaces even when its code shares none of your words. This is added
recall, not reordering: doc answers are appended below the existing code results
and the top ten never move. It applies to `search` and `--smart`; the core MCP
surface's `search_context` retrieves over chunk text only.

Add `--smart` to rewrite the query and re-rank the results, which helps on vague
questions. Plain `search` never spawns anything, so it stays fast enough for an
agent to call on every turn.

Embeddings always come from the code embedder compiled into the binary
(`potion-code-16M`, a static model distilled from `bge-base-en-v1.5` and tuned
for code). There is nothing to download, no daemon, no API key, and no cache: it
works on first run and fully offline, identically on every machine. It is also
the fast path: roughly 650 chunks/second in-process versus about 47 through a
local Ollama embed model, which is the difference between a semantic index that
builds in two minutes and one that takes half an hour on a large repo.

Optionally add query rewrite and re-ranking (the `--smart` half), which helps on
vague questions. That step genuinely needs an LLM, so it uses a local
[Ollama](https://ollama.com) model, still no cloud and no API key. Pick a tier
with `--tier fast|smart|max`:

| tier | assist model | needs |
|---|---|---|
| fast | `gemma3:1b` | runs anywhere, CPU ok |
| smart | `gemma4:e2b` | about 10 GB VRAM |
| max | `gemma4:e4b` | about 16 GB VRAM |

Or borrow an installed coding-agent CLI for that same rewrite and re-rank step
with `--ai-provider agent` (it autodetects a cheap tier like `claude -p --model
haiku`; override with `--ai-command`). Both are optional: without either, vector
plus full-text search still answers every query. Over MCP, an agent can also pass
`rerank: true` to have its own model reorder results in-process.

## Supported formats

Go, Rust, Java, Kotlin, Ruby, C#, PHP, Dart, Elixir, TypeScript/TSX, Lua, Python,
JavaScript, Bash, Fish, C/C++, QML, CSS/SCSS, Markdown, TOML, YAML, JSON/JSONC,
INI, and Hyprland (`hyprlang`), plus a line-based reader for everything else
(sway/i3, rofi `rasi`, polybar, kitty, dunst, and similar).

## More

- [Architecture](docs/ARCHITECTURE.md): how indexing, the graph, and the servers fit together
- [Measuring token usage](docs/TOKENS.md): how the savings number is computed, and how to check it
- [Changelog](CHANGELOG.md)

Built with Go, Tree-sitter, and SQLite for Linux, macOS, and Windows.
