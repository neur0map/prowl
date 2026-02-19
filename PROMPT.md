# OpenClaw Viz - Development Brief

## Project Overview

OpenClaw Viz is a desktop application that provides a live, visual representation of an AI agent's "mind" - the files, memory, and code that make up its workspace. The app renders these files as an interactive node graph and animates them in real-time as the agent reads, writes, and executes code.

Think of it as a window into an AI's thought process. When the agent reads a memory file, that node glows. When it writes to a file, that node pulses. When it calls a tool, the corresponding source file lights up. The user watches the graph come alive as the agent works.

## Target Users

Developers and enthusiasts running OpenClaw (an AI agent framework) who want to understand what their agent is doing under the hood. Secondary audience includes anyone interested in visualizing AI agent execution for demos, debugging, or education.

## Technical Stack

- Electron for cross-platform desktop support
- Vue 3 with Composition API for the frontend
- Vue Flow for graph rendering
- Pinia for state management
- Chokidar for file system watching
- TypeScript throughout

The project scaffold already exists with this structure in place.

## Core Functionality

### 1. Workspace Visualization

The app connects to a local directory (the OpenClaw workspace) and builds a graph representation of its contents. Each file becomes a node. Directories can be represented as parent nodes or groupings. The graph should be navigable - users can pan, zoom, and click nodes to inspect them.

Files are color-coded by type:
- Markdown files (memory, documentation) in blue
- TypeScript/JavaScript in their conventional colors
- JSON configuration files in amber
- Directories in purple

The layout should be clean and readable. Nodes should not overlap. The initial layout can use a tree structure based on the file hierarchy, but consider force-directed positioning for a more organic feel.

### 2. Live Activity Detection

This is the core feature. The app must detect when files are being accessed and update the visualization in real-time.

For file writes, this is straightforward - use file system watchers to detect changes.

For file reads, this is harder since operating systems don't easily expose read events. The current approach is to parse OpenClaw's log output, which records tool calls including file reads. The parser watches the log file and extracts relevant events.

When activity is detected:
- The corresponding node should visually activate (glow, pulse, or similar effect)
- The activation should be temporary (fade after 1-2 seconds)
- Different activity types should have different visual treatments (read vs write)
- Multiple simultaneous activations should all be visible

### 3. Tool Execution Tracking

OpenClaw uses various tools (exec, read, write, web_search, etc). When the agent invokes a tool, this should be reflected in the visualization. If we can map tool calls to source files (the code that implements the tool), those nodes should also light up.

This creates a two-layer visualization:
- The agent's workspace files (memory, configs, skills)
- The underlying code being executed

### 4. User Interface

The interface should be minimal and focused on the graph. A sidebar provides:
- Input field for workspace path
- Input field for log file path
- Connect/disconnect controls
- Legend explaining the color coding
- Basic stats (node count, active nodes)

A status bar shows connection state and current activity.

The overall aesthetic should be dark, with the graph as the focal point. Think developer tools meets data visualization. No clutter.

## Visual Design Goals

The visualization should feel alive. Subtle ambient motion (nodes gently floating or breathing) makes the static state interesting. Activity animations should be satisfying - a pulse that ripples outward, a glow that fades smoothly.

Edges between nodes should be clean. Consider animated dashes flowing along edges when data moves between connected files.

Performance matters. The graph should remain smooth with hundreds of nodes. Animations should not cause jank.

## What Success Looks Like

A user launches the app, points it at their OpenClaw workspace, and sees their agent's files rendered as a beautiful graph. They start a conversation with their agent in another window. As the agent thinks, they watch nodes light up - first SOUL.md as the agent reads its personality, then memory files as it recalls context, then TOOLS.md as it figures out what to do. When the agent runs a command, the exec tool lights up. When it writes to memory, that node glows orange.

The user gains intuition about how their agent works. They can see which files matter, which are accessed frequently, and how information flows.

## Current State

The project scaffold is complete with:
- Electron main process with file watcher and log parser
- Preload script exposing IPC bridge
- Vue renderer with graph components
- Basic store for state management
- CSS variables for theming

The foundation works but needs refinement:
- Graph layout algorithm needs improvement
- Activity animations need polish
- Log parsing needs to handle more patterns
- Edge rendering could be more sophisticated
- The UI could use refinement

## Constraints

- Must work offline (no external API calls for core functionality)
- Must run on macOS, Windows, and Linux
- Should handle workspaces with 500+ files without performance issues
- No heavy dependencies beyond what's already specified

## Out of Scope (For Now)

- Editing files from within the app
- Multiple simultaneous workspaces
- Cloud sync or sharing
- Mobile versions
- 3D visualization (possible future addition)

## Development Priorities

1. Get the basic flow working end-to-end: connect to workspace, render graph, see activity
2. Polish the visual design and animations
3. Improve log parsing accuracy
4. Optimize performance for larger workspaces
5. Add quality-of-life features (search, filtering, node details panel)
