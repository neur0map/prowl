# OpenClaw Viz

Live visualization of OpenClaw's workspace, memory files, and execution. Watch your agent's "mind" light up as it thinks.

## What It Does

- Displays workspace files as an interactive node graph
- Highlights nodes in real-time when files are read or written
- Shows tool activations as they happen
- Parses OpenClaw logs to track execution flow

## Architecture

```
src/
  main/           Electron main process
    index.ts      App entry, window management, IPC handlers
    watcher.ts    File system watcher (chokidar)
    parser.ts     Log file parser for tool detection

  preload/        Context bridge between main and renderer
    index.ts      Exposes safe API to renderer

  renderer/       Vue frontend
    components/
      graph/      Graph visualization components
      ui/         Interface components (sidebar, status bar)
    stores/       Pinia state management
    styles/       Global CSS and variables
```

## Requirements

- Node.js 18+
- npm or pnpm

## Setup

```bash
# Install dependencies
npm install

# Run in development
npm run dev

# Build for production
npm run build
```

## Usage

1. Launch the app
2. Enter your OpenClaw workspace path (e.g., `~/.openclaw/workspace`)
3. Click Connect to load the file graph
4. Optionally, enter a log file path to watch tool activations
5. Files will glow green when read, orange when written

## Node Types

| Type       | Color  | Description            |
|------------|--------|------------------------|
| Markdown   | Blue   | .md files              |
| TypeScript | Blue   | .ts files              |
| JavaScript | Yellow | .js files              |
| JSON       | Amber  | .json files            |
| YAML       | Gray   | .yaml, .yml files      |
| Directory  | Purple | Folders                |

## Activity Indicators

- **Green glow**: File is being read
- **Orange glow**: File is being written
- **Pulse animation**: Recently active

## Configuration

The app watches these file types by default:
- `.md`, `.ts`, `.js`, `.json`, `.yaml`, `.yml`

Ignored paths:
- `node_modules/`
- `.git/`
- `dist/`

## Development

```bash
# Type checking
npm run typecheck

# Linting
npm run lint
```

## How It Works

1. **File Watcher**: Uses chokidar to monitor the workspace directory. Emits events when files are added, changed, or removed.

2. **Log Parser**: Tails the OpenClaw log file, parsing lines for tool invocations. Matches patterns like `Read`, `Write`, `exec` to identify which files are being accessed.

3. **IPC Bridge**: Main process sends events to the renderer via Electron IPC. The preload script exposes a safe API.

4. **Graph Rendering**: Vue Flow renders the file tree as an interactive graph. Node positions are calculated based on directory depth. Active nodes receive CSS animations.

## Limitations

- File read detection relies on log parsing, not actual file access monitoring
- Large workspaces may have performance implications
- Currently single-workspace only

## License

MIT
