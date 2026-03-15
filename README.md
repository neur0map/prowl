<div align="center">

# Prowl

**Context compiler for AI coding agents.**

Prowl parses your codebase into a structured graph — symbols, call edges, communities, embeddings — and serves it over [MCP](https://modelcontextprotocol.io). One tool call replaces the entire exploration phase.

[![License](https://img.shields.io/badge/license-BSL--1.0-green.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.21+-00ADD8.svg)](https://go.dev)
[![MCP](https://img.shields.io/badge/MCP-compatible-blue.svg)](https://modelcontextprotocol.io)

[Install](#install) · [Quick Start](#quick-start) · [Why Prowl](#the-challenges) · [5 Tools](#the-5-tools) · [How It Works](#how-it-works)

</div>

---

## Install

### Prerequisites

- **Go 1.21+** — [install Go](https://go.dev/dl/)

### Build from source

```bash
go install github.com/neur0map/prowl/cmd/prowl@latest
```

Or clone and build:

```bash
git clone https://github.com/neur0map/prowl.git
cd prowl
go build -o ~/go/bin/prowl ./cmd/prowl/
```

### Add Go to your PATH

The `prowl` binary is installed to `~/go/bin/`. Make sure it's in your PATH.

**Temporary** (current shell only):

```bash
export PATH="$HOME/go/bin:$PATH"
```

**Permanent** (add to your shell profile):

```bash
# zsh (macOS default)
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc

# bash
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

Verify:

```bash
prowl --help
```

---

## Quick Start

### Setup wizard (new project)

```bash
cd your-project
prowl
```

If no `.prowl/` directory exists, prowl launches a setup wizard that walks you through:

1. **Confirm directory** — verify the project path
2. **Ignore patterns** — add extra ignores beyond defaults (node_modules, .git, vendor, etc.)
3. **Embedding model** — download the semantic search model (~90MB, once) or skip
4. **MCP integration** — auto-detects Claude Code and Codex, installs prowl as an MCP server with one keypress. Shows raw JSON config for Cursor and other IDEs.
5. **Indexing** — runs the 8-phase pipeline with a progress bar

### Dashboard (indexed project)

```bash
cd your-project
prowl
```

If `.prowl/` exists, prowl opens a terminal dashboard with 3 tabs:

- **Stats** — file/symbol/edge/embedding counts, language breakdown, community list, last indexed time
- **Search** — semantic search across your codebase (type a query, get ranked results)
- **Daemon** — live file watcher that keeps the index fresh as you edit. Auto-starts with the dashboard.

**Keyboard:** `tab`/`1-3` switch tabs · `s` toggle daemon · `j/k` navigate · `q` quit

### MCP server (for AI agents)

```bash
prowl mcp /path/to/project
```

This starts the MCP JSON-RPC server on stdin/stdout. You don't run this directly — it's invoked by AI coding tools via their MCP config.

**Claude Code:**

```bash
claude mcp add -s user prowl -- prowl mcp /path/to/project
```

**Codex:**

```bash
codex mcp add prowl -- prowl mcp /path/to/project
```

**Cursor / other IDEs** — add to your MCP config:

```json
{
  "mcpServers": {
    "prowl": {
      "command": "prowl",
      "args": ["mcp", "/absolute/path/to/your-project"]
    }
  }
}
```

> First run downloads the embedding model (~90MB, once, to `~/.prowl/models/`).

---

## The Challenges

AI coding agents spend most of their token budget *finding* code, not *writing* it. Five problems make this worse:

- **Blind Exploration** — The agent has no map. It greps, reads, greps again, reads more. Each file costs tokens. Most files turn out to be irrelevant.

- **Missing Relationships** — Reading a file tells you what's *in* it, not what *depends* on it. The agent can't see callers, upstream consumers, or community membership without manually tracing imports across dozens of files.

- **No Ranking Signal** — When grep returns 15 matches, which 3 matter most? The agent reads all of them. There's no relevance score, no structural priority, no way to say "start here."

- **Repeated Discovery** — Every new task starts from scratch. The agent re-explores the same codebase, re-reads the same files, re-traces the same dependencies. Nothing is cached between sessions.

- **Invisible Blast Radius** — Before editing a file, the agent should know what breaks. Without a dependency graph, it either checks nothing (risky) or reads everything (expensive).

## How Prowl Solves Each One

| Challenge | Prowl Solution |
|:----------|:---------------|
| **Blind Exploration** → | `prowl_scope` — semantic search + graph expansion returns exactly the files needed for a task. One call, not fifteen. |
| **Missing Relationships** → | `prowl_file_context` — every file comes with its exports, signatures, calls, callers, imports, upstream, and community. The full neighborhood. |
| **No Ranking Signal** → | Scope results are ranked by semantic similarity, community cohesion, session heat, and sorted by dependency depth. Depth 0 = read first. |
| **Repeated Discovery** → | `prowl_overview` — agent's first call on any project. Returns the full map: file counts, language breakdown, community clusters with member digests, detected processes. Instant orientation. |
| **Invisible Blast Radius** → | `prowl_impact` — given a file (and optionally a symbol), returns all direct and transitive dependents, affected communities, and whether the change crosses community boundaries. |

---

## Token Savings

The numbers below are based on measured file sizes (avg ~1,200-2,500 tokens per source file at ~4 chars/token), constructed prowl response payloads, and published research on agent exploration patterns.

### prowl_scope vs. raw exploration

| Metric | Without Prowl | With Prowl |
|:-------|:-------------|:-----------|
| Tokens for 10 files | ~19,000 (reading raw content) | ~1,300 (scope response with full context) |
| Tool calls | 5-15 (grep, read, grep, read...) | 1 |
| Irrelevant reads | 30-50% of files turn out unused* | 0 (graph-ranked results) |
| Compression ratio | — | **~14x** vs raw reads |

\* [Developer tracking data](https://dev.to/egorfedorov/i-tracked-where-my-claude-code-tokens-actually-go-37-were-wasted-2gll) — out of every 10 files an agent reads, only 6-7 actually matter for the task.

### prowl_overview vs. manual orientation

| Metric | Without Prowl | With Prowl |
|:-------|:-------------|:-----------|
| Tokens to understand project structure | ~15,000-30,000 (ls, grep, read key files) | ~950 (overview response) |
| Tool calls | 5-10 | 1 |
| Compression ratio | — | **~16-32x** |

### What published research says

Graph-guided code exploration is a studied approach. The closest published benchmark:

- **[LocAgent (ACL 2025)](https://arxiv.org/html/2503.09089v1)** — graph-guided LLM agent for code localization on SWE-bench. Graph tools reduced cost by **86%** ($0.66 → $0.09 per issue) while improving file-level localization accuracy to **92.7%**. Removing the graph traversal tool dropped accuracy by 4.4%.

### Before & after

```
Without Prowl (typical agent exploration):
  Agent thinks: "Where is the auth logic?"
    → grep "auth"                 (~300 tokens, results to scan)
    → read 8 matching files       (~12,000 tokens, most irrelevant)
    → grep "login"                (~200 tokens)
    → read 5 more files           (~8,000 tokens)
    → trace imports manually      (~3,000 tokens)
  Total: ~24,000 tokens · 10+ tool calls

With Prowl:
  Agent calls: prowl_scope({ task: "fix the auth login flow" })
    → 10 files, ranked by relevance, sorted by dependency depth
    → each file includes: exports, signatures, calls, callers, community
  Total: ~1,300 tokens · 1 tool call
```

> **Note:** The before/after example uses estimated token counts based on typical file sizes. Actual savings depend on codebase size, file lengths, and how many exploration rounds the agent needs. The 14x compression ratio is measured from real response payloads vs. raw file content.

---

## The 5 Tools

### `prowl_overview` → Map the territory

Agent's first call. Returns the full project topology so it can orient without reading a single file.

```
Files: 68 · Symbols: 389 · Edges: 17 · Embeddings: 63
Languages: { go: 48, rust: 6, typescript: 14 }

Communities:
  [components]
    src/components/RunnerNode.tsx: components | 1 exports, 1 calls, 0 callers
    src/lib/terminal.ts: lib | 6 exports, 0 calls, 3 callers
  [dashboard]
    templates/dashboard/dashboard.go: dashboard | 5 exports, 1 calls, 0 callers
    templates/dashboard/ui.go: dashboard | 3 exports, 0 calls, 1 callers

Processes:
  list_templates (4 steps)
  get_coder_context (4 steps)
```

Each community member is a **glance digest** — path, parent directory, export/call/caller counts — enough to decide whether to drill in, at ~15 tokens per file.

### `prowl_scope` → Find exactly what's needed

The power tool. Describe a task in natural language. Prowl combines semantic search with 1-hop graph expansion, ranks by community cohesion and session heat, then sorts by dependency depth.

```json
prowl_scope({ "task": "fix the template runner", "limit": 8 })
```

```
depth=0  src/lib/terminal.ts              1-hop · 6 exports
depth=0  src-tauri/src/lib.rs             1-hop · 4 exports
depth=1  src/components/RunnerNode.tsx     search hit · score: 0.85
depth=1  src-tauri/src/main.rs            search hit · score: 0.80
depth=2  src/components/Canvas.tsx         1-hop · imports RunnerNode
```

**Depth ordering** tells the agent *what to read first* — depth 0 files have no in-set dependencies. Read them, then depth 1, then depth 2. No backtracking.

Each file includes: exports, signatures, calls, callers, imports, upstream, and community.

### `prowl_file_context` → Deep-dive one file

Full structural context for a single file. Use when the agent already knows which file it needs.

```json
prowl_file_context({ "path": "templates/dashboard/dashboard.go" })
```

```
community: dashboard (id: 1)
exports:   struct Model, func New, method Init, method Update, method View
calls:     templates/dashboard/ui.go
callers:   (none)
imports:   (none)
upstream:  (none)
```

### `prowl_impact` → Know what breaks

Blast radius analysis before making changes. Returns direct dependents, transitive dependents, affected communities, and whether the change crosses community boundaries.

```json
prowl_impact({ "path": "src/lib/terminal.ts" })
```

```
Direct dependents (3):
  src/components/RunnerNode.tsx   [CALLS]
  src/components/TerminalNode.tsx [CALLS]
  src/components/CoderNode.tsx    [CALLS]

Transitive dependents (1):
  src/components/Canvas.tsx  via CoderNode.tsx

Affected communities: [components]
Cross-community: false
```

Optional `symbol` parameter narrows the analysis to a specific function or type.

### `prowl_semantic_search` → Find by meaning

When the agent doesn't know the file path. Vector similarity search over embedded signatures.

```json
prowl_semantic_search({ "query": "terminal input handling", "limit": 5 })
```

```
1. src/lib/terminal.ts (score: 0.82)
   function createTerminal(), function sendInput(), ...
2. src/components/TerminalNode.tsx (score: 0.79)
   function TerminalNode()
3. src-tauri/src/main.rs (score: 0.71)
   fn main()
```

---

## What Prowl Builds

```
your-project/
└── .prowl/
    ├── prowl.db                  # SQLite: files, symbols, edges, embeddings, communities
    └── context/
        ├── src/auth.ts/
        │   ├── .exports           # handleLogin, validateToken
        │   ├── .signatures        # export function handleLogin(req: Request): Promise<User>
        │   ├── .calls             # src/db.ts, src/crypto.ts
        │   ├── .callers           # src/routes/login.ts
        │   ├── .imports           # src/db.ts
        │   ├── .upstream          # src/routes/login.ts
        │   └── .community         # auth (id: 2)
        ├── _meta/
        │   ├── communities.txt    # Louvain clusters with member lists
        │   └── processes.txt      # Detected multi-step call chains
        └── ...
```

The `context/` directory is plain text. Agents can also read it directly via filesystem — no MCP required.

---

## How It Works

Prowl runs an 8-phase pipeline on your source code:

```
 Source Files
      │
      ▼
 ┌─────────────┐
 │  1. Scan     │  Walk file tree, respect .gitignore
 └──────┬───────┘
        ▼
 ┌─────────────┐
 │  2. Parse    │  Extract symbols via tree-sitter (functions, classes, types, exports)
 └──────┬───────┘
        ▼
 ┌─────────────┐
 │  3. Imports  │  Resolve import statements to target files
 └──────┬───────┘
        ▼
 ┌─────────────┐
 │  4. Calls    │  Match function calls to definitions across files
 └──────┬───────┘
        ▼
 ┌─────────────┐
 │  5. Heritage │  Trace class inheritance and interface implementations
 └──────┬───────┘
        ▼
 ┌──────────────────┐
 │  6. Communities   │  Detect clusters of related files (Louvain algorithm)
 └──────┬────────────┘
        ▼
 ┌──────────────────┐
 │  7. Processes     │  Detect multi-step call chains from entry points
 └──────┬────────────┘
        ▼
 ┌──────────────────┐
 │  8. Embed         │  Generate vector embeddings (Snowflake Arctic Embed S, 384-dim)
 └──────┬────────────┘
        ▼
   .prowl/prowl.db + .prowl/context/
```

Everything runs locally. No external services, no API keys, no network calls after the one-time model download.

---

## Live Updates

The daemon watches your project for file changes and incrementally updates the index. It auto-starts when you open the dashboard, or you can toggle it with `s` on the Daemon tab.

When a file is saved:
- Re-parses symbols, re-resolves calls and imports
- Updates SQLite and re-embeds if signatures changed
- Rewrites affected `.prowl/context/` files
- Cascades updates to callers and upstream files

After 30 seconds of idle, it re-runs community detection and process detection across the entire graph.

---

## Supported Languages

| Language | Extensions |
|:---------|:-----------|
| Go | `.go` |
| TypeScript | `.ts`, `.tsx` |
| JavaScript | `.js`, `.jsx` |
| Rust | `.rs` |
| Python | `.py` |
| Java | `.java` |
| C# | `.cs` |
| Swift | `.swift` |
| C++ | `.cpp`, `.cc`, `.cxx` |
| C | `.c`, `.h` |

---

## Key Concepts

<details>
<summary><strong>Community Detection</strong></summary>

Prowl runs the Louvain algorithm on the file-level call/import graph to detect **communities** — clusters of files that are more connected to each other than to the rest of the codebase. Communities typically correspond to subsystems: `auth`, `api`, `database`, `ui`.

Community membership powers two features:
- **Overview digests** — files are grouped by community so agents see the project's natural boundaries
- **Scope ranking** — when searching for files relevant to a task, files in the same community as a search hit get a ranking boost. This surfaces structurally related code that keyword search would miss.

</details>

<details>
<summary><strong>Dependency Depth</strong></summary>

When `prowl_scope` returns files, each one has a `depth` field computed via topological sort (Kahn's algorithm) over the result set's internal dependency edges.

- **Depth 0** = leaf files. They don't depend on anything else in the result set. Read these first.
- **Depth 1** = depends on depth-0 files. Read after depth 0.
- **Depth N** = depends on depth-(N-1) files.

This gives the agent a **reading order** — no backtracking, no "I should have read that file first."

Files in dependency cycles get the same depth (max + 1).

</details>

<details>
<summary><strong>Session Heat</strong></summary>

Prowl tracks which files the agent accesses during a session. Files accessed more recently and more frequently get a higher **heat score** (sigmoid + exponential decay, 1-hour half-life).

Heat is blended into scope ranking at 15% weight. This means files the agent is actively working with float higher in subsequent searches — the tool adapts to the agent's focus without any explicit configuration.

Heat is in-memory only. It resets when the MCP server restarts.

</details>

<details>
<summary><strong>Glance Digests</strong></summary>

In `prowl_overview`, each community member is shown as a one-line **glance digest**:

```
src/auth.ts: auth | 3 exports, 5 calls, 2 callers
```

Format: `path: parent_dir | N exports, N calls, N callers`

This costs ~15 tokens per file. An agent scanning 100 files in overview spends ~1,500 tokens to see the full project structure — versus reading every file raw. The digest tells the agent *whether* to drill in, not *how*.

</details>

---

## Requirements

- Go 1.21+
- ~90MB disk for the embedding model (downloaded once to `~/.prowl/models/`)

## License

[Boost Software License 1.0](LICENSE)
