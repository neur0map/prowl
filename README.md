<div align="center">

# Prowl

**Your AI agent is reading 50 files to answer one question. It doesn't have to.**

Prowl indexes your codebase into a structured graph with embeddings — then serves it over MCP. One tool call replaces the entire exploration phase.

[![License](https://img.shields.io/badge/license-BSL--1.0-green.svg)](LICENSE)

</div>

---

## The Problem

Every time an AI coding agent needs to understand your project, it does this:

```
Agent thinks: "Where is the auth logic?"
  → grep for "auth"           (300 tokens)
  → read 8 matching files     (12,000 tokens)
  → grep for "login"          (200 tokens)
  → read 5 more files         (8,000 tokens)
  → trace imports manually    (3,000 tokens)
  → finally has enough context

Total: ~25,000 tokens, 15+ tool calls, 40 seconds
```

Prowl pre-computes all of this. The agent asks once and gets a complete answer:

```
Agent calls: prowl_scope("fix the auth login flow")
  → 5 files ranked by relevance, with exports,
    signatures, call graph, and dependency depth

Total: ~1,500 tokens, 1 tool call, instant
```

## Install

```bash
go install github.com/neur0map/prowl/cmd/prowl@latest
```

First run downloads the embedding model (~70MB, once).

## Usage

**Index a project:**

```bash
prowl index /path/to/project
```

This creates a `.prowl/` directory with the SQLite database and context files. Takes seconds for most projects.

**Start MCP server:**

```bash
prowl mcp /path/to/project
```

**Connect to Claude Code** — add to `~/.claude.json`:

```json
{
  "mcpServers": {
    "prowl": {
      "command": "prowl",
      "args": ["mcp", "/path/to/your/project"]
    }
  }
}
```

Restart Claude Code. Run `/mcp` to verify — you should see `prowl` with 5 tools.

## What Prowl Builds

```
your-project/
└── .prowl/
    ├── prowl.db              # SQLite: files, symbols, edges, embeddings, communities
    └── context/
        ├── src/auth.ts/
        │   ├── .exports       # handleLogin, validateToken
        │   ├── .signatures    # export function handleLogin(req: Request): Promise<User>
        │   ├── .calls         # src/db.ts, src/crypto.ts
        │   ├── .callers       # src/routes/login.ts
        │   ├── .imports       # src/db.ts
        │   ├── .upstream      # src/routes/login.ts
        │   └── .community     # auth (id: 2)
        ├── _meta/
        │   ├── communities.txt
        │   └── processes.txt
        └── ...
```

The context directory is plain text — agents can also read it directly without MCP.

## The 5 Tools

### `prowl_overview`

First call on any project. Returns file/symbol/edge counts, language breakdown, community clusters with member digests, and detected processes.

```
Files: 102, Symbols: 930, Edges: 377, Embeddings: 96
Languages: {"go": 87, "typescript": 15}
Communities:
  [mcp] cmd/prowl/main.go: prowl | 0 exports, 9 calls, 2 callers
        internal/mcp/server.go: mcp | 4 exports, 3 calls, 5 callers
  [parser] internal/parser/parser.go: parser | 3 exports, 5 calls, 7 callers
Processes:
  index_pipeline (8 steps)
  mcp_tool_dispatch (5 steps)
```

### `prowl_scope`

The power tool. Given a task description, returns exactly the files needed — combining semantic search with 1-hop graph expansion, community-aware ranking, and dependency depth ordering.

```json
prowl_scope({ "task": "fix the template runner", "limit": 8 })
```

Returns files sorted by depth (read depth-0 files first, they have no dependencies):

```
depth=0  src/lib/terminal.ts        (6 exports, called by RunnerNode)
depth=0  src-tauri/src/lib.rs       (4 exports, called by main.rs)
depth=1  src/components/RunnerNode.tsx  (search hit, score: 0.85)
depth=2  src/components/Canvas.tsx   (imports RunnerNode)
```

Each file includes exports, signatures, calls, callers, imports, upstream, and community.

### `prowl_file_context`

Full context for a single file. Use when you know which file you need.

```json
prowl_file_context({ "path": "src/auth.ts" })
```

### `prowl_impact`

Blast radius analysis. Before editing a file, find everything that would be affected.

```json
prowl_impact({ "path": "src/lib/terminal.ts" })
```

```
Direct dependents:
  src/components/RunnerNode.tsx  [CALLS]
  src/components/TerminalNode.tsx [CALLS]
  src/components/CoderNode.tsx   [CALLS]
Transitive dependents:
  src/components/Canvas.tsx via CoderNode.tsx
Affected communities: [components]
Cross-community: false
```

### `prowl_semantic_search`

Find code by meaning when you don't know the file path.

```json
prowl_semantic_search({ "query": "password hashing", "limit": 5 })
```

## How It Works

Prowl runs an 8-phase pipeline:

1. **Scan** — walk the file tree, respect `.gitignore`
2. **Parse** — extract symbols (functions, classes, types, exports) via tree-sitter
3. **Resolve imports** — map import statements to target files
4. **Resolve calls** — match function calls to their definitions across files
5. **Heritage** — trace class inheritance chains
6. **Communities** — detect clusters of related files via Louvain algorithm
7. **Processes** — detect multi-step call chains (entry points → flows)
8. **Embed** — generate vector embeddings for semantic search (Snowflake Arctic Embed S)

Everything is stored in a single SQLite database. No external services, no API keys, no network calls.

## Supported Languages

Go, TypeScript, JavaScript, Rust, Python, Java, C#, Swift, C++, C

## Requirements

- Go 1.21+
- ~70MB disk for the embedding model (downloaded on first use to `~/.prowl/models/`)

## License

[Boost Software License 1.0](LICENSE)
