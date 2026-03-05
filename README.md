<div align="center">

<img src="public/media/prowl_logo.png" alt="Prowl" width="128" />

# Prowl

**Your second monitor while AI writes the code.**

Your AI coder is editing 40 files across 6 directories. You're staring at a scrolling terminal, hoping nothing important got touched. You have no picture of what just happened.

Prowl builds a live architecture map of your entire project and rebuilds it as files change. You see what changed, what's connected, and what might break — and your AI can query the same graph to skip reading hundreds of files.

[![Version](https://img.shields.io/github/v/tag/neur0map/prowl?label=version)](https://github.com/neur0map/prowl/releases)
[![Beta](https://img.shields.io/badge/status-beta-orange.svg)](#status)
[![License](https://img.shields.io/badge/license-BSL--1.0-green.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/neur0map/prowl/pulls)

</div>

---

## Quick Start

```
1. Download from Releases → install
2. Open Prowl → drop your project folder
3. Start your AI coder (in Prowl's terminal or your own)
4. Watch the map rebuild as files change
```

### [Download for macOS (Apple Silicon)](https://github.com/neur0map/prowl/releases/latest) · [macOS (Intel)](https://github.com/neur0map/prowl/releases/latest) · [Windows](https://github.com/neur0map/prowl/releases/latest) · [Linux](https://github.com/neur0map/prowl/releases/latest)

Works with **any AI coder**: Claude Code, Cursor, Codex, Gemini CLI, Aider, Windsurf — anything that edits files.

<div align="center">

<img src="public/media/focus-graph.png" alt="Prowl Focus Graph" width="800" />

<br />

<a href="https://www.youtube.com/watch?v=zMJWWZqRlX0">
  <img src="https://img.youtube.com/vi/zMJWWZqRlX0/maxresdefault.jpg" alt="Prowl Demo" width="800" />
</a>

<sub>Click to watch on YouTube</sub>

</div>

---

## Why Prowl

| Without Prowl | With Prowl |
|---------------|------------|
| Staring at terminal output, guessing what changed | See your architecture map live-reindex as files change |
| AI reads 40 files to understand your project (~100K tokens) | AI calls `prowl_overview` and gets the full picture (~1K tokens) |
| "What breaks if I refactor this?" — grep, read 30 files, pray | `prowl_impact("UserService", "upstream")` — complete answer, 1 call |
| No idea which parts of the codebase the AI just touched | Affected clusters glow in real-time as your AI works |
| Want to compare against another repo? Clone it, open two editors | Paste a GitHub URL, browse it side-by-side — no clone needed |

**In our benchmark on a 76-file Chrome extension, Claude used ~271K tokens without Prowl vs ~5.9K with Prowl MCP — a 98% reduction.** The bigger your codebase, the more you save. [Full benchmark →](docs/mcp-benchmark.md)

---

## Is This For You?

You'll get the most out of Prowl if:

- You use **Claude Code, Cursor, Codex, or any AI coder** daily
- Your project has more than a handful of files
- You want to **see the blast radius** before your AI commits a refactor
- You're tired of your AI **wasting tokens** re-reading the same files
- You want a **local-first** tool — no code leaves your machine

You don't need to be a programmer to use it. If you can drag a folder onto an app, you can use Prowl.

---

## What You Get

### Live Architecture Map
Your project as clustered cards grouped by zone (frontend, backend, config, etc.). The graph **live-reindexes as files change** — you see AI edits land in real-time. Clusters glow when your AI agent touches related files.

### Built-in Terminal + Chat
Run Claude Code, Aider, or any tool right inside Prowl. Ask questions about your codebase without burning tokens in your main AI. Sessions persist across restarts, context auto-compacts so you never hit the limit.

### MCP: A Graph Brain for Your AI
19 tools your AI agent can call instead of reading hundreds of files. Architecture overview, blast radius analysis, semantic search, change detection, repo comparison — one tool call instead of thirty. **~98% fewer tokens.** [Details below →](#mcp-let-your-ai-coder-use-prowls-brain)

### Compare Mode
Paste a GitHub URL to load any repo alongside your project. Browse files, grep code, compare approaches — all via REST API with no disk clone. Auto-closes after 30 minutes of inactivity.

### Change Detection
Map your uncommitted git changes to affected symbols, clusters, and a risk level. Know what you're about to break before you commit.

### Everything Else
- **Code viewer** — Click any cluster to browse files and edit in-place
- **Smart search** — Find code by meaning, not just keywords (semantic embeddings)
- **Snapshot restore** — Re-open projects instantly, git-aware incremental re-indexing
- **10 languages** — JS, TS, Python, Java, Go, Rust, C, C++, C#, Swift — structs, enums, traits, impls, macros, and more
- **7 AI providers** — OpenAI, Anthropic, Gemini, Azure, Ollama (local), OpenRouter, Groq
- **Works offline** — Everything runs locally. No data leaves your machine.

---

## Example: Safe AI Refactor

```
1. Open your project in Prowl. Start Claude Code in Prowl's terminal.

2. Tell Claude: "Refactor UserService to use dependency injection."

3. Watch Prowl's map — the Auth and API clusters glow as Claude
   edits files. You see exactly what's being touched.

4. Before committing, Claude calls prowl_impact("UserService", "upstream")
   → sees 3 clusters affected, 12 upstream consumers.

5. Claude calls prowl_changes(scope: "all")
   → 5 files changed, risk level "medium", 2 clusters impacted.

6. You see the full picture. Commit with confidence.
```

---

## MCP: Let Your AI Coder Use Prowl's Brain

<div align="center">

**Prowl has a built-in [Model Context Protocol](https://modelcontextprotocol.io) server.**

Your AI coding agent connects to Prowl and queries the knowledge graph directly — instead of reading hundreds of files to understand your project.

</div>

### The Problem

Every time an AI coder needs to understand your project, it reads files. Lots of files. Each file read costs tokens — and tokens cost money. In a large codebase, your AI burns most of its budget just figuring out where things are.

<div align="center">
<img src="public/media/no-mcp-example.png" alt="Without MCP — excessive file reads" width="700" />
<img src="public/media/no-mcp-example2.png" alt="Without MCP — token waste" width="700" />
</div>

```
You: "What breaks if I refactor UserService?"

AI without Prowl:    grep → read 30 files → trace imports → read more files
                     Result: ~100,000 tokens, 30+ tool calls, maybe misses things

AI with Prowl:       prowl_impact("UserService", "upstream")
                     Result: ~1,000 tokens, 1 tool call, complete answer
```

### Measured Results

> **Full breakdown:** [MCP Benchmark: Real Token Savings on a Real Project](docs/mcp-benchmark.md) — Claude Code analyzed a 76-file Chrome extension with and without Prowl.
> - **Full project understanding:** ~84,795 tokens vs ~8,035 tokens. **90.5% reduction.**
> - **Delegated research (8 questions):** ~134,500 tokens vs ~9,114 tokens. **93.2% reduction.**

| What the AI needs | Without Prowl | With Prowl MCP | Saved |
|:------------------|:-------------:|:--------------:|:-----:|
| Understand project architecture | Read all files | `prowl_overview` | **96%** |
| Blast radius of a refactor | Recursive grep + read chain | `prowl_impact` | **98%** |
| Find code by meaning | Multiple greps + read matches | `prowl_search` | **95%** |
| Answer a codebase question | 10+ searches + file reads | `prowl_ask` | **99%** |
| Deep multi-step investigation | 20+ reads + manual reasoning | `prowl_investigate` | **97%** |

<details>
<summary><strong>Full benchmark data</strong></summary>

Actual bytes returned by Prowl MCP vs total project file contents (257,902 bytes):

| Tool | MCP Response | Without MCP (est.) | Reduction |
|------|:-----------:|:-----------------:|:---------:|
| `prowl_status` | 101 B | N/A | — |
| `prowl_overview` | 11,090 B | 257,902 B | 95.7% |
| `prowl_hotspots` | 467 B | 257,902 B | 99.8% |
| `prowl_search` | 2,428 B | ~50,000 B | 95.1% |
| `prowl_grep` | 27 B | ~500 B | 95.2% |
| `prowl_context` | 1,556 B | 257,902 B | 99.4% |
| `prowl_cypher` | 358 B | ~5,000 B | 92.9% |
| `prowl_explore` | 170 B | ~15,816 B | 98.9% |
| `prowl_impact` | 954 B | ~40,000 B | 97.6% |
| `prowl_read_file` | 744 B | 744 B | 0% |
| `prowl_ask` | 1,200 B | ~80,000 B | 98.5% |
| `prowl_investigate` | 4,500 B | ~120,000 B | 96.2% |

Token approximation: 1 token ~ 4 bytes.

</details>

### What About Prowl's Own AI Cost?

`prowl_ask` and `prowl_investigate` use Prowl's internal AI agent. Your AI coder only pays for the final answer.

| Prowl's LLM | `ask` cost | `investigate` cost |
|:-------------|:----------:|:------------------:|
| **Ollama (local)** | $0.00 | $0.00 |
| **Groq** | $0.006 | $0.022 |

With Ollama, the research is free — Prowl's AI runs on your machine while your expensive cloud AI gets a compact, pre-researched answer.

### 19 Tools

| Tool | What it does |
|:-----|:-------------|
| `prowl_status` | Health check — is Prowl running with a project loaded? |
| `prowl_search` | Hybrid keyword + semantic search |
| `prowl_cypher` | Direct Cypher queries against the knowledge graph |
| `prowl_grep` | Regex search across all indexed source files |
| `prowl_read_file` | Read full source code with fuzzy path matching |
| `prowl_overview` | High-level map: clusters, processes, dependencies |
| `prowl_explore` | Drill into a symbol, cluster, or process |
| `prowl_impact` | Blast radius for any function, class, or file |
| `prowl_context` | Project stats, hotspots, directory tree |
| `prowl_hotspots` | Most connected symbols |
| `prowl_ask` | Ask Prowl's AI a question (internal agent) |
| `prowl_investigate` | Multi-step research (deeper, multiple internal tools) |
| `prowl_changes` | Map git diffs to affected symbols, clusters, risk level |
| `prowl_compare` | Load a GitHub repo for side-by-side comparison |
| `prowl_compare_file_tree` | Browse the comparison repo's file tree |
| `prowl_compare_read_file` | Read a file from the comparison repo |
| `prowl_compare_grep` | Regex search across cached comparison files |
| `prowl_compare_summary` | Stats for the loaded comparison repo |
| `prowl_summary` | Alias for `prowl_overview` |

### Setup

**One-click** — Open Prowl Settings → MCP Server → **Configure Claude Code**. Restart Claude Code.

**Manual** — Add to `~/.claude.json`:

```json
{
  "mcpServers": {
    "prowl": {
      "type": "stdio",
      "command": "node",
      "args": ["/path/to/Prowl/dist/mcp-server.js"]
    }
  }
}
```

**Verify** — Run `/mcp` in Claude Code. You should see Prowl with 19 tools.

---

## Install

Go to **[Releases](https://github.com/neur0map/prowl/releases/latest)** and grab the right file:

| System | File |
|--------|------|
| Mac (Apple Silicon — M1/M2/M3/M4) | `Prowl-x.x.x-mac-arm64.dmg` |
| Mac (Intel) | `Prowl-x.x.x-mac-x64.dmg` |
| Windows | `Prowl-x.x.x-win-x64-setup.exe` |
| Linux | `Prowl-x.x.x-linux-x86_64.AppImage` or `.deb` |

<details>
<summary><strong>macOS first launch (not code-signed yet)</strong></summary>

**Option A:** Right-click the app → Open → click Open in the dialog. macOS asks about keychain access — click Always Allow.

**Option B:** Remove quarantine flag:
```bash
xattr -rd com.apple.quarantine /Applications/Prowl.app
```

**Option C:** [Build from source](#for-developers) if you don't trust unsigned binaries.

You only need to do this once. Code signing is coming soon.

</details>

---

## Status

Prowl is in **beta**. It works well for daily use, but expect rough edges.

**What works:** Full indexing pipeline, live reindexing, MCP server, chat, terminal, compare mode, snapshot persistence, all 10 languages.

**What's rough:** No code signing yet (macOS quarantine workaround needed). Large repos (1000+ files) can be slow on first index. Some UI polish still in progress.

**What's next:** VS Code extension, GitHub PR integration, native indexer daemon, custom themes.

[Report issues](https://github.com/neur0map/prowl/issues) · [Request features](https://github.com/neur0map/prowl/issues) · [@neur0map on X](https://x.com/neur0map)

---

## Screenshots

<img src="public/media/focus-graph.png" alt="Focus Graph" width="700" />

<img src="public/media/code.png" alt="Code Inspector" width="700" />

---

<details>
<summary><strong>For Developers</strong></summary>

### Building from Source

**Prerequisites:** Node.js 18+, Python 3.x with setuptools, Git

```bash
git clone https://github.com/neur0map/prowl.git
cd prowl
npm install
npm run dev
```

### Packaging

```bash
npm run dist:mac       # macOS DMG
npm run dist:win       # Windows installer
npm run dist:linux     # Linux AppImage/deb
npm run dist           # All platforms
```

### Python note

If you're on Python 3.12+ and get a `distutils` error:

```bash
pip3 install setuptools
```

### Tech Stack

| Category | Technology |
|----------|------------|
| Framework | Electron, React, TypeScript |
| Styling | Tailwind CSS v4 |
| Graph | React Flow, graphology |
| Parsing | tree-sitter WASM |
| Database | KuzuDB WASM |
| Embeddings | Snowflake Arctic Embed XS (WebGPU/WASM) |
| AI | LangChain |
| Terminal | xterm.js, node-pty |
| Editor | Monaco |
| Build | electron-vite |

</details>

---

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=neur0map/prowl&type=Date)](https://star-history.com/#neur0map/prowl&Date)

---

## Contributing

PRs welcome. Open an issue first for major changes.

---

## License

[Boost Software License 1.0](LICENSE)
