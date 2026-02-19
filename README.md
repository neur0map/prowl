# Prowl

Prowl is a desktop application that transforms codebases into interactive knowledge graphs. Load any repository and explore its structure -- files, functions, classes, imports, and call relationships -- rendered as a navigable graph you can query with natural language. When you load a local folder, Prowl automatically watches for file changes, highlighting nodes in real time as AI coding agents (Claude Code, Cursor, Windsurf, etc.) edit your code.

## Features

### Code Ingestion

Three ways to load a codebase:

- **ZIP upload** -- Drag and drop or browse for a `.zip` archive of any repository.
- **GitHub clone** -- Paste a GitHub URL to clone directly in the browser. Supports public repositories and private repositories via a personal access token (PAT). Powered by isomorphic-git running entirely client-side.
- **Local folder** (Electron only) -- Select a folder through the native macOS directory picker. The app reads all source files, builds the knowledge graph, and automatically starts a live file watcher on the loaded path.

### Knowledge Graph

The ingestion pipeline parses source code with web-tree-sitter and builds a typed knowledge graph with the following node types:

| Node Type   | Description                                |
|-------------|--------------------------------------------|
| Project     | Root node for the loaded codebase          |
| Folder      | Directory in the file tree                 |
| File        | Source file                                |
| Function    | Function or arrow function definition      |
| Class       | Class definition                           |
| Method      | Class method                               |
| Interface   | Interface or type alias                    |
| Enum        | Enum definition                            |
| Import      | Import declaration                         |
| Variable    | Exported variable or constant              |
| Community   | Detected module cluster (Leiden algorithm) |
| Process     | Detected execution flow across symbols     |

Relationships include `CONTAINS`, `DEFINES`, `IMPORTS`, `CALLS`, `INHERITS`, `IMPLEMENTS`, `EXTENDS`, `OVERRIDES`, `DECORATES`, `USES`, `MEMBER_OF`, and `STEP_IN_PROCESS`.

The graph is stored in KuzuDB WASM, enabling Cypher queries directly in the browser. It is rendered with Sigma.js (graphology) using a force-directed layout (ForceAtlas2 / dagre).

### Supported Languages

- JavaScript / TypeScript
- Python
- Java
- C / C++
- C#
- Go
- Rust

### Live File Watcher

When a local folder is loaded, Prowl automatically starts a chokidar-based file watcher on the workspace directory. Any file change -- whether from an AI agent, your editor, or a build tool -- triggers a real-time highlight on the corresponding graph node. Nodes glow briefly on write events, giving you a live view of which parts of the codebase are being touched.

No manual connection step is required. The watcher starts as soon as the folder ingestion pipeline completes.

### AI Chat (Graph RAG)

The right panel includes an AI chat interface powered by LangChain. The agent has access to seven tools that operate over the knowledge graph and source files:

| Tool      | Description                                                  |
|-----------|--------------------------------------------------------------|
| search    | Hybrid search (BM25 + semantic + Reciprocal Rank Fusion)     |
| cypher    | Execute Cypher queries against KuzuDB                        |
| grep      | Regex pattern search across all loaded files                 |
| read      | Read a specific file by path                                 |
| overview  | Codebase map showing clusters and processes                  |
| explore   | Deep dive on a symbol, cluster, or process                   |
| impact    | Impact/blast-radius analysis for a given symbol              |

Every factual claim in the agent's responses is grounded with file and line citations that link back to nodes in the graph.

#### Supported LLM Providers

Configure any of the following in the Settings panel:

- **OpenAI** -- GPT-4o, GPT-4o-mini, GPT-4 Turbo, etc.
- **Anthropic** -- Claude Sonnet, Claude Opus, etc.
- **Google Gemini** -- Gemini 2.0 Flash, Gemini 1.5 Pro, etc.
- **Azure OpenAI** -- Any Azure-hosted deployment.
- **Ollama** -- Local models (Llama, Mistral, etc.) via `localhost:11434`.
- **OpenRouter** -- Access any model through the OpenRouter API.

### Semantic Search

Prowl generates vector embeddings for all code symbols using Transformers.js (snowflake-arctic-embed-xs, 22M parameters, 384 dimensions). Embedding runs locally in the browser:

- **WebGPU** -- Preferred backend, hardware-accelerated on supported browsers.
- **WASM** -- Automatic fallback when WebGPU is unavailable.

Search combines BM25 keyword scoring with semantic similarity via Reciprocal Rank Fusion (RRF), the same approach used by Elasticsearch and Pinecone.

### UI Design

Prowl follows an Apple Liquid Glass design aesthetic:

- Translucent glass panels with `backdrop-filter: blur(40px) saturate(180%)`
- macOS native vibrancy (`under-window`), hidden titlebar, and traffic light positioning
- Outfit font for UI text, JetBrains Mono for code
- Muted, desaturated node colors; system-blue accents
- Minimal animations -- opacity transitions only, no pulsing or glowing effects

## Architecture

```
electron/
  main.ts              Electron main process, IPC handlers, native dialogs
  preload.ts           Context bridge exposing prowl API to renderer
  watcher.ts           Chokidar file watcher (workspace monitoring)
  parser.ts            Log file parser for tool call detection

src/
  App.tsx              Root component, view routing (onboarding/loading/exploring)
  main.tsx             React entry point

  components/
    DropZone.tsx        Onboarding screen (ZIP / GitHub / Folder tabs)
    GraphCanvas.tsx     Sigma.js graph renderer with zoom/pan controls
    Header.tsx          Top bar with search, AI toggle, settings
    RightPanel.tsx      Tabbed panel (code inspector, AI chat, agent events)
    AgentPanel.tsx      Agent event feed and watcher status
    FileTreePanel.tsx   Left sidebar file tree
    StatusBar.tsx       Bottom status bar with graph stats
    SettingsPanel.tsx   LLM provider configuration modal
    CodeReferencesPanel.tsx  Source code viewer with line highlighting
    QueryFAB.tsx        Floating action button for Cypher queries

  config/
    ignore-service.ts   File/directory ignore rules
    supported-languages.ts  Tree-sitter language registry

  core/
    graph/              Knowledge graph data structure and types
    ingestion/          Multi-phase parsing pipeline
      pipeline.ts       Orchestrator (extract -> parse -> resolve -> enrich)
      parsing-processor.ts   AST parsing with tree-sitter
      import-processor.ts    Import/export resolution
      call-processor.ts      Function call graph extraction
      heritage-processor.ts  Class inheritance chains
      community-processor.ts Leiden community detection
      process-processor.ts   Execution flow detection
    kuzu/               KuzuDB WASM adapter and schema
    llm/                LangChain agent, tools, and provider config
    embeddings/         Transformers.js embedding pipeline
    search/             BM25 index and hybrid search (RRF)
    tree-sitter/        Parser loader and WASM grammar management

  hooks/
    useAppState.tsx     Global application state (React context)
    useSigma.ts         Sigma.js instance management
    useAgentWatcher.ts  File watcher integration and node highlighting
    useSettings.ts      LLM settings persistence (localStorage)

  services/
    git-clone.ts        Isomorphic-git clone (public + PAT auth)
    zip.ts              JSZip extraction

  lib/
    graph-adapter.ts    KnowledgeGraph -> graphology conversion
    mermaid-generator.ts  Mermaid diagram export
    constants.ts        Shared constants
    utils.ts            Utility functions

  workers/              Web Worker entry points
  vendor/               Vendored algorithms (Leiden)
```

## Requirements

- Node.js 18+
- npm or pnpm
- macOS (for native vibrancy and traffic light features; the renderer works cross-platform)

## Setup

```bash
# Install dependencies
npm install

# Run in development (launches Electron with hot reload)
npm run dev

# Build for production
npm run build

# Preview production build
npm run preview
```

## Tech Stack

| Layer           | Technology                                              |
|-----------------|---------------------------------------------------------|
| Desktop shell   | Electron 29, electron-vite                              |
| Frontend        | React 18, TypeScript, Tailwind CSS v4                   |
| Graph rendering | Sigma.js 3, graphology, ForceAtlas2, dagre              |
| Code parsing    | web-tree-sitter (WASM grammars per language)            |
| Graph database  | KuzuDB WASM (Cypher queries, vector index)              |
| Git operations  | isomorphic-git, lightning-fs                             |
| AI / LLM        | LangChain (LangGraph ReAct agent), multi-provider       |
| Embeddings      | Transformers.js (snowflake-arctic-embed-xs), WebGPU/WASM|
| Search          | MiniSearch (BM25), RRF hybrid fusion                    |
| File watching   | chokidar                                                |
| Bundling        | Vite 5, vite-plugin-wasm, vite-plugin-top-level-await   |

## License

MIT
