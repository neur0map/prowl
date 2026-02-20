# Prowl

You're building software with AI agents. Claude Code, Cursor, Codex, Windsurf -- whatever the tool, the experience is the same. You tell it what to build, it goes off and edits a bunch of files, and you're left staring at a stream of diffs trying to figure out what just happened to your codebase.

Prowl exists because that's a terrible way to work.

## What it does

Prowl turns a codebase into an interactive knowledge graph. Every file, function, class, import, and call relationship becomes a node you can see and click. When an AI agent edits your code, the affected nodes light up in real time. You stop reading diffs and start watching your software change.

It also has a built-in AI chat that actually understands your code's structure. Not "paste your code and ask questions" -- the agent queries a graph database, runs hybrid search across your symbols, and grounds every answer with file and line citations that link back to the graph. You can ask "what calls this function" and get a real answer traced through your call graph, not a guess.

## Who it's for

Developers who use AI coding agents and want to understand what those agents are doing. If you vibe code and sometimes wonder whether the AI just rewired half your app, this is for you.

Also useful if you're joining a new team and need to understand a large codebase fast. Load the repo, look at the graph, ask questions. The community detection clusters related modules together and the process detection traces execution flows, so you can see how features work end-to-end without reading every file.

## The problem

AI coding tools are powerful but opaque. They operate on files -- creating, editing, deleting -- but the developer sees those changes one file at a time. There's no spatial awareness. You can't see that when the agent edited `auth.ts`, it also changed three files that import from it, and one of those files is your payment flow.

Code is a graph. Files import other files. Functions call other functions. Classes inherit from other classes. But every tool we have -- editors, terminals, file trees -- shows code as a flat list. Prowl shows the actual structure.

## How it works

Load a codebase three ways: drop a ZIP, paste a GitHub URL, or pick a local folder. Prowl parses every source file with tree-sitter, extracts all symbols and relationships, runs community detection (Leiden algorithm) to cluster modules, and traces execution flows through the call graph. The result is a navigable, queryable knowledge graph rendered with force-directed layout.

When you load a local folder, a file watcher starts automatically. Four detection layers run in parallel:

- **Filesystem watcher** -- catches file writes, additions, removals from any process
- **Agent log parser** -- reads Claude Code JSONL logs for structured tool call events
- **Process monitor** -- polls for which processes have files open (macOS/Linux)
- **WebSocket client** -- receives real-time events from OpenClaw workspaces

All of these feed into the graph. When something changes, the corresponding node pulses. You see your codebase react to the agent's work in real time.

## Features

**Knowledge graph** -- Nodes for files, folders, functions, classes, methods, interfaces, enums, variables. Edges for imports, calls, inheritance, containment, membership. Stored in KuzuDB WASM so you can run Cypher queries directly.

**AI chat** -- LangChain ReAct agent with seven tools: hybrid search (BM25 + semantic + RRF), Cypher queries, regex grep, file reader, codebase overview, symbol explorer, and impact analysis. Every response cites sources. Clicking a citation opens the file and highlights the node.

**Semantic search** -- Local embeddings via Transformers.js (snowflake-arctic-embed-xs). Runs on WebGPU when available, falls back to WASM. Combined with BM25 keyword search through Reciprocal Rank Fusion.

**Integrated terminal** -- xterm.js with node-pty. Run your agents here and watch the graph respond. Split panes, multiple tabs, persisted sessions.

**Code editor** -- Monaco editor overlay on the graph. Click any node to read the source. Autosave, syntax highlighting, file tabs. For quick edits, not a full IDE.

**Multi-provider LLM support** -- OpenAI, Anthropic, Google Gemini, Azure OpenAI, Ollama (local), OpenRouter. API keys encrypted via OS keychain (Electron safeStorage).

**Community detection** -- Leiden algorithm clusters related symbols into modules. The graph colors by community so you can visually identify architectural boundaries.

**Process detection** -- Traces execution flows from entry points through the call graph. Shows how features work across files and modules.

**Seven languages** -- JavaScript, TypeScript, Python, Java, C, C++, C#, Go, Rust. All parsed with tree-sitter WASM grammars.

## Setup

```bash
npm install
npm run dev
```

Requires Node.js 18+ and macOS for native vibrancy. The renderer works cross-platform.

`npm run build` for production. `npm run preview` to test the build.

## Tech stack

Electron, React, TypeScript, Tailwind CSS v4. Sigma.js and graphology for graph rendering with ForceAtlas2 layout. Web-tree-sitter for code parsing. KuzuDB WASM for the graph database. LangChain/LangGraph for the AI agent. Transformers.js for embeddings. xterm.js and node-pty for the terminal. Monaco for the editor. Vite for bundling.

## Status

Active development. The graph, chat, terminal, editor, and watcher are functional. OAuth sign-in for Claude and OpenAI is scaffolded but waiting on provider support for desktop app flows.

## License

MIT
