# Prowl: Mission Control for AI Coding

**Date:** 2026-02-19
**Status:** Design approved

---

## Vision

Prowl is a codebase intelligence tool — not an IDE. It turns any repo into an interactive knowledge graph and lets you watch AI agents work in real-time. This design adds three capabilities that eliminate context-switching for vibe coders: an integrated terminal, a light code editor, and a refreshed chat experience with OAuth support.

**Tagline:** See your AI think.

**The three-layer model:**

| Layer | Purpose | Writes files? | AI involved? |
|---|---|---|---|
| Chat (Graph RAG) | Understand the codebase | No | Yes |
| Editor (Monaco) | Light manual edits | Yes (manual only) | No |
| Terminal (xterm.js) | AI-driven building | Yes (via agents) | Yes (Claude Code / Codex) |

---

## 1. Overall Layout

```
+-----------------------------------------------------------+
|  Header                                                   |
|  [P Prowl] [Project Name] | [Cmd+K Search] | [Gear] [>>] |
+--------+----------------------------+---------------------+
|  File  |                            |  Right Panel (40%)  |
|  Tree  |     Graph Canvas           |                     |
|        |                            |  [Chat] [Processes] |
|        |  +------------------+      |  [Agent]            |
|        |  | Code Editor      |      |                     |
|        |  | (overlay,        |      |  Chat messages      |
|        |  |  resizable)      |      |  + tool call pills  |
|        |  +------------------+      |  + input bar        |
|        |                            |                     |
+--------+----------------------------+---------------------+
|  Status Bar          [node/edge counts]    [Ctrl+` Term]  |
+-----------------------------------------------------------+
|  +-- Terminal Drawer (hidden by default) ---------------+  |
|  | [* zsh] [* claude] [* codex  x]     [+] [||] [^] [x]|  |
|  |--------------------------------------------------------|  |
|  |  ~/prowl/openclaw-viz  main !?  v25.6.0  17:16       |  |
|  |  > _                                                  |  |
|  +-------------------------------------------------------+  |
+-----------------------------------------------------------+
```

**Key principles:**
- Graph canvas remains the center focus at all times
- Terminal is hidden until needed — no wasted vertical space
- Code editor overlays the graph (left side), same slot as the existing CodeReferencesPanel
- Right panel is unchanged structurally — tabbed Chat/Processes/Agent
- Everything uses the existing glass morphism design language

---

## 2. Terminal Drawer

### Activation
- **Toggle:** `Ctrl+`` (backtick) — opens and focuses last-used tab, or closes if already open
- **New tab shortcut:** `Ctrl+Shift+`` — creates a new terminal tab directly
- **Status bar button:** Click the terminal icon in the status bar to toggle
- Drawer stays alive in background when hidden — processes keep running

### Tab Bar

```
+----------------++----------------++----------------+
| * zsh          || * claude       || * codex    x   |  [+] [||] [^v] [x]
+----------------++----------------++----------------+
```

- Each tab shows: **colored dot** + **process name** + **close button** (on hover)
  - Green dot: active process running
  - Gray dot: idle/awaiting input
- Process name auto-detected from the running command (zsh, claude, codex, node, npm, etc.)
- Active tab: `glass-elevated` background
- Inactive tabs: `glass-subtle` background
- **[+]** — new terminal tab, cwd defaults to project root
- **[||]** — split active tab into two side-by-side panes (max 2 panes per tab)
- **[^v]** — drag handle to resize drawer height
- **[x]** — close entire drawer (terminals stay alive in background)

### Split Panes
- Clicking [||] divides the active tab into two horizontal panes
- Thin vertical divider (2px, rgba(255,255,255,0.08))
- Focused pane gets a subtle 1px accent-blue top border
- Use case: run `claude` in left pane, watch logs/tests in right pane

### Sizing
- Minimum height: 150px
- Maximum height: 60% of window height
- Height persisted in localStorage between toggles
- Smooth slide-up animation: 200ms ease-out

### Styling
- Background: `var(--color-void)` (#1c1c1e)
- Top border: `1px solid rgba(255,255,255,0.08)`
- Tab bar: `glass-subtle` backdrop
- Font: `JetBrains Mono` (matches existing monospace)
- Respects user's shell theme (powerlevel10k, starship, oh-my-zsh) — xterm.js renders ANSI natively

### Implementation
- **xterm.js** in the renderer with the `WebGL` addon for GPU-accelerated rendering
- **node-pty** spawns shell processes from Electron main process
- IPC bridge via preload:
  - `prowl.terminal.create(cwd)` — spawn a new PTY, returns terminal ID
  - `prowl.terminal.write(id, data)` — send input to PTY
  - `prowl.terminal.resize(id, cols, rows)` — resize PTY
  - `prowl.terminal.kill(id)` — kill PTY process
  - `prowl.terminal.onData(id, callback)` — receive output from PTY

---

## 3. Code Editor Panel

### Position
Replaces the existing `CodeReferencesPanel` overlay on the left side of the graph canvas. Same resize handle, same position, now upgraded to a full Monaco editor.

```
+-----------------------------------------------------------+
|  Graph Canvas                                             |
|  +-- Code Editor (overlay) ---------+                     |
|  | [Login.tsx] [README.md]     [x]  |                     |
|  |----------------------------------|                     |
|  |  1  import React from 'react'    |                     |
|  |  2                               |                     |
|  |  3  export function Login() {    |                     |
|  |  4    const [email, setEmail] =  |                     |
|  |  5      useState('')             |                     |
|  |  ...                             |                     |
|  |                                  |# <-- resize handle  |
|  |              Saved checkmark     |#                    |
|  +----------------------------------+                     |
+-----------------------------------------------------------+
```

### Opening the Editor
- **Click a file node** in the graph — editor opens with that file
- **Click a function/class node** — opens the file, scrolls to that symbol's line number
- **Click a code-ref link** in the chat — opens file at referenced line
- If editor is already open, clicking another node opens a new tab (or focuses existing tab if file is already open)

### Tab Bar
- File tabs showing basename + language icon
- Modified indicator: subtle dot when unsaved changes exist (brief, since autosave fires quickly)
- Close button on hover per tab
- Middle-click to close a tab
- Max ~6 visible tabs, then horizontal scroll

### Autosave
- **Debounced at 1.5 seconds** after user stops typing
- Status indicator in bottom-right corner of editor:
  - Typing: nothing shown (clean)
  - Saving: "Saving..." in `text-muted` (brief flash)
  - Saved: "Saved (checkmark)" in `text-secondary`, fades out after 2 seconds
  - Error: "Save failed" in red, persists until next successful save
- File written through Electron IPC: `prowl.fs.writeFile(path, content)`
- External change detection: if the file watcher detects a change on disk, a banner appears: "File changed on disk. Reload?" with [Reload] [Ignore] buttons

### Editor Configuration
- Monaco Editor, `vs-dark` base theme customized to Prowl palette:
  - Background: `var(--color-deep)` (#2c2c2e)
  - Line numbers: `var(--color-text-muted)`
  - Selection: `rgba(10, 132, 255, 0.25)` (accent blue at 25%)
  - Cursor: `var(--color-accent)` (#0A84FF)
- Font: `JetBrains Mono` (matches terminal)
- Minimap: off (this is for light edits, not deep coding sessions)
- Line numbers: on
- Word wrap: on
- Read-only mode for binary files or files > 1MB (shows a notice)

### Resize Behavior
- Drag handle on the right edge (same as current CodeReferencesPanel)
- Width stored in localStorage (persists between sessions)
- Min width: 350px
- Max width: 50% of canvas width
- Graph reflows around the overlay

### Keyboard Shortcuts
- `Cmd+S` — force save immediately (muscle memory, even though autosave exists)
- `Cmd+W` — close active tab
- `Cmd+P` — quick file open (reuses the header Cmd+K search)
- `Escape` — close entire editor panel, return focus to graph

---

## 4. Chat UX Refresh

The chat stays in the right panel's Chat tab. No structural changes — just a UX refresh with collapsed tool calls and cleaner message flow.

### Message Layout

```
+------------------------------------------+
|                                          |
|  -- You -------------------------------- |
|  How does authentication work            |
|  in this codebase?                       |
|                                          |
|  -- Prowl ------------------------------ |
|                                          |
|  [magnifier] Searched graph  check 120ms |
|  [document]  Read auth.ts    check  45ms |
|                                          |
|  Authentication is handled by the        |
|  `AuthService` class in                  |
|  `src/services/auth.ts`. It uses JWT     |
|  tokens stored in...                     |
|                                          |
|  > auth.ts:14 -- AuthService             |
|  > middleware/auth.ts:8 -- verify        |
|                                          |
+------------------------------------------+
```

### Tool Call Pills
- Slim single-line pills: icon + action name + status + duration
- Status states:
  - **Running:** pulsing accent-blue dot, active verb ("Searching graph...")
  - **Done:** checkmark icon, duration in muted text ("120ms")
  - **Failed:** red dot, "Failed" label
- Click any pill to expand inline — shows inputs/outputs in a `glass-subtle` box
- Click again to collapse
- Multiple tool calls stack vertically with 4px gap

### Grounding Links
- Displayed as clickable `> filename:line -- symbolName` references
- Click opens the file in the code editor panel at that line
- Hover shows a 3-line code preview tooltip
- Also highlights the corresponding node in the graph with a brief pulse animation

### Message Styling
- **User messages:** `glass-subtle` background, "You" label
- **Assistant messages:** no background (clean), "Prowl" label
- **Markdown:** rendered with existing `chat-prose` styles
- **Code blocks:** `JetBrains Mono`, `var(--color-deep)` background, copy button on hover

### Input Bar
- Sticky at the bottom of the right panel
- Auto-resizing textarea: 1 line default, max 6 lines
- `Enter` to send, `Shift+Enter` for newline
- Send button doubles as Stop button when streaming
- Placeholder: "Ask about your codebase..." in `text-muted`

### Suggestion Chips (empty state only)
- Shown when conversation is empty
- 3 default suggestions:
  - "What does this codebase do?"
  - "Show me the main entry points"
  - "Find the most complex modules"
- Disappear after first message

### Streaming Behavior
- Tool call pills appear one by one as they fire
- Text streams word-by-word below the pills
- Grounding links appear at the end once the response completes
- Auto-scroll follows the stream, but stops if user scrolls up manually

---

## 5. Settings Panel with OAuth

The existing SettingsPanel keeps its provider icon strip + dynamic form design. OAuth "Sign in" buttons are added for providers that support it (Claude and OpenAI).

### Provider Strip (unchanged)
```
[Claude] [OpenAI] [Gemini] [Ollama] [OpenRouter]
```
Icon buttons, active state highlight. Same as today.

### Provider Forms

| Provider | OAuth button | API Key field | Extra fields |
|---|---|---|---|
| Claude | "Sign in with Claude" | Yes (below, as fallback) | Model, Endpoint |
| OpenAI | "Sign in with OpenAI" | Yes (below, as fallback) | Model, Base URL |
| Gemini | None | Yes | Model, Endpoint |
| Ollama | None | None | Base URL, Model, [Test Connection] |
| OpenRouter | None | Yes | Model (live fetch), Endpoint |

### Layout for OAuth Providers (Claude, OpenAI)

```
+----------------------------------------------+
|                                              |
|  +----------------------------------------+  |
|  |  (cloud) Sign in with Claude           |  |
|  +----------------------------------------+  |
|                                              |
|  -------------------- or ----------------    |
|                                              |
|  API Key                                     |
|  +-------------------------------+ +---+     |
|  | sk-ant-********************   | | e |     |
|  +-------------------------------+ +---+     |
|                                              |
|  Model                                       |
|  +-------------------------------+           |
|  | claude-sonnet-4-20250514   v  |           |
|  +-------------------------------+           |
|                                              |
|  Endpoint (optional)                         |
|  +-------------------------------+           |
|  | https://api.anthropic.com     |           |
|  +-------------------------------+           |
|                                              |
+----------------------------------------------+
```

- The "or" divider makes it clear: OAuth **or** API key, not both
- If user has both configured, OAuth takes priority

### OAuth Flow

1. User clicks "Sign in with Claude" (or "Sign in with OpenAI")
2. Electron opens system browser via `shell.openExternal(authUrl)` to the provider's OAuth consent page
3. Prowl registers a custom deep-link protocol handler: `prowl://oauth/callback`
4. User approves in the browser
5. Browser redirects to `prowl://oauth/callback?code=...`
6. Electron main process intercepts the deep link
7. Main process exchanges the authorization code for access + refresh tokens
8. Tokens stored in **system keychain** via Electron's `safeStorage` API (not localStorage)
9. Settings panel updates to show connected state

### Connected State

When OAuth is active, the sign-in button is replaced with:

```
+----------------------------------------------+
|  (check) Connected as alex@email.com  [Sign out] |
+----------------------------------------------+
```

- Green checkmark + email/username from OAuth profile
- "Sign out" clears tokens from keychain, reverts to sign-in button
- API key field below is disabled/grayed: "Using OAuth -- key not needed"
- If OAuth token expires, Prowl silently refreshes using the refresh token
- If refresh fails, shows a subtle banner: "Session expired. Sign in again."

### Non-OAuth Providers

- **Gemini / OpenRouter:** API key field + model selector, same as today
- **Ollama:** Base URL + Model + "Test Connection" button with green/red status dot

### Security Footer

```
+----------------------------------------------+
| (lock) API keys stored locally only.         |
|        OAuth tokens stored in system         |
|        keychain.                             |
+----------------------------------------------+
```

Displayed at the bottom of the settings panel. `text-muted` styling with a lock icon.

---

## 6. New Dependencies

| Package | Purpose | Size impact |
|---|---|---|
| `xterm` | Terminal emulator in renderer | ~300KB |
| `@xterm/addon-webgl` | GPU-accelerated terminal rendering | ~50KB |
| `@xterm/addon-fit` | Auto-fit terminal to container | ~5KB |
| `node-pty` | Native PTY spawning (Electron main) | Native module |
| `@monaco-editor/react` | Monaco editor React wrapper | ~2MB (loaded async) |

All other dependencies (LangChain, Sigma.js, Mermaid, etc.) remain unchanged.

---

## 7. New IPC Bridge (preload additions)

```typescript
// Terminal
prowl.terminal.create(cwd: string): Promise<string>        // returns terminal ID
prowl.terminal.write(id: string, data: string): void
prowl.terminal.resize(id: string, cols: number, rows: number): void
prowl.terminal.kill(id: string): void
prowl.terminal.onData(id: string, cb: (data: string) => void): void

// File system (additions for editor)
prowl.fs.writeFile(path: string, content: string): Promise<void>
prowl.fs.onFileChange(path: string, cb: () => void): void  // external change detection
```

---

## 8. Implementation Order

1. **Terminal drawer** — xterm.js + node-pty + IPC bridge + tab management + split panes
2. **Code editor** — Monaco integration replacing CodeReferencesPanel + autosave + file tabs
3. **Chat UX refresh** — collapsed tool call pills + grounding links + streaming polish
4. **OAuth integration** — deep-link protocol + token exchange + keychain storage + connected state UI
5. **Wiring** — graph node click opens editor, chat links open editor, terminal activity feeds agent watcher

Each phase is independently shippable.

---

## 9. What This Is NOT

- **Not an IDE.** No extensions, no language servers, no debugger, no git UI.
- **Not competing with Cursor/Zed.** The graph is the navigation model, not a file tree.
- **Chat never writes files.** It reads, searches, analyzes. File mutations happen through the terminal (AI agents) or the editor (manual).
- **The editor is intentionally basic.** Minimap off, no autocomplete, no IntelliSense. Fix a typo, update a config, tweak a string. For real coding, use the terminal.
