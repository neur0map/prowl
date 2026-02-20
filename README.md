# Prowl

Interactive knowledge graph for codebases. Parses source code into a navigable graph of files, functions, classes, and their relationships. Watches for changes in real time.

## Quick start

```bash
npm install
npm run dev
```

Requires Node.js 18+.

## Loading a codebase

Three ways to load code:

- **Local folder** -- pick a directory, Prowl parses it and starts watching for changes
- **GitHub URL** -- paste a repo URL, Prowl clones and parses it
- **ZIP file** -- drag and drop

Prowl uses tree-sitter to extract symbols (functions, classes, imports, calls) and builds a graph database (KuzuDB) you can query directly with Cypher.

## Live watching

When loaded from a local folder, Prowl watches for file changes and updates the graph in real time. Four detection layers run in parallel:

- Filesystem watcher (file writes, additions, removals)
- Claude Code log parser (structured tool call events from JSONL logs)
- Process file monitor (polls which processes have files open)
- WebSocket client (OpenClaw workspace events)

Changed nodes pulse on the graph so you can see what's being modified.

## AI chat

Built-in chat agent that queries the graph database and runs hybrid search (BM25 + semantic via Transformers.js) across your codebase. Tools available to the agent:

- Hybrid search (keyword + embedding + reciprocal rank fusion)
- Cypher graph queries
- Regex grep
- File reader
- Codebase overview
- Symbol explorer
- Impact analysis

Responses include file/line citations that link back to nodes on the graph. Supports OpenAI, Anthropic, Gemini, Azure OpenAI, Ollama, and OpenRouter. API keys encrypted via OS keychain (Electron safeStorage).

## Other features

- **Integrated terminal** -- xterm.js + node-pty, multiple tabs
- **Code editor** -- Monaco, click any node to read source
- **Community detection** -- Leiden algorithm clusters related symbols, graph colors by community
- **Process tracing** -- traces execution flows from entry points through the call graph
- **Languages** -- JavaScript, TypeScript, Python, Java, C, C++, C#, Go, Rust

## Building

```bash
npm run build      # production build
npm run preview    # test the build
```

## Tech stack

Electron, React, TypeScript, Tailwind CSS v4, Sigma.js, graphology, web-tree-sitter, KuzuDB WASM, LangChain, Transformers.js, xterm.js, node-pty, Monaco, Vite.

## License

MIT
