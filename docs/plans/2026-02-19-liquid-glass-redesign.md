# Prowl UI Redesign: Apple Liquid Glass

## Color System

### Backgrounds
- `--color-void`: `#1c1c1e` (Apple system gray 6)
- `--color-deep`: `#2c2c2e`
- `--color-surface`: `rgba(255,255,255,0.06)`
- `--color-elevated`: `rgba(255,255,255,0.10)`
- `--color-hover`: `rgba(255,255,255,0.14)`

### Glass
- Fill: `rgba(255,255,255,0.08)`
- Border: `1px solid rgba(255,255,255,0.15)`
- Backdrop: `blur(40px) saturate(180%)`
- Elevated glass: `rgba(255,255,255,0.12)` fill, `blur(60px)`

### Text
- Primary: `#f5f5f7`
- Secondary: `rgba(255,255,255,0.55)`
- Muted: `rgba(255,255,255,0.35)`

### Accent
- Blue: `#0A84FF` (Apple dark mode blue)
- Blue hover: `#409CFF`
- Green: `#30D158` (system green, status only)
- Red: `#FF453A` (system red, errors only)
- Orange: `#FF9F0A` (system orange, warnings only)

### Node Colors (slightly desaturated)
- File: `#5A9CF5`
- Folder: `#8085EC`
- Class: `#E0A243`
- Function: `#3EBD8C`
- Interface: `#D96FA0`
- Import: `#7A7F8A`
- Method: `#3AADA0`

### Borders
- Subtle: `rgba(255,255,255,0.08)`
- Default: `rgba(255,255,255,0.15)`

### Shadows (no glows)
- Elevation low: `0 1px 4px rgba(0,0,0,0.2)`
- Elevation med: `0 4px 16px rgba(0,0,0,0.25)`
- Elevation high: `0 8px 32px rgba(0,0,0,0.35)`

## Sizing and Typography

- Border radius: 8px cards, 6px inputs/buttons, 4px badges
- Font: Outfit 400-500 for UI, JetBrains Mono for code
- Body text: 13px. Labels: 11px. Headings: 15px max.
- Icons: 16px in headers, 14px in controls
- Tighter padding: py-2 px-3 for buttons, py-1.5 px-2.5 for inputs

## Animations

- Remove: `animate-breathe`, `pulse-glow`, `shadow-glow`
- Keep: `transition: all 0.2s ease` on interactive elements
- Status dots: `opacity` transition only, no pulsing
- Panel entrances: `opacity 0->1` over 150ms, no slides

## Floating Agent Bar (Dynamic Island) -- REMOVED

**Status: Removed.** The FloatingAgentBar was removed in favor of auto-watcher
behavior on local folder load. When a user loads a local folder via the DropZone,
the file watcher starts automatically as soon as the ingestion pipeline completes
(see `handleFolderLoad` in `App.tsx`). There is no longer a manual connect/disconnect
UI for the watcher. Agent activity events are displayed in the AgentPanel within the
RightPanel instead.

Original design (archived for reference):
- Position: absolute, top-center of graph canvas
- Disconnected: 400px wide glass pill, path input + browse + Connect button
- Connected: expands to ~500px, green dot + "Watching" + event ticker + Disconnect
- Click to expand: drops down to ~300px panel with full event log + log path
- Glass material with `backdrop-filter: blur(40px) saturate(180%)`

## Component Changes

### Header
- Logo: `#f5f5f7` diamond, "Prowl" weight-400
- Search: glass input, no ring glow, subtle focus border
- AI button: glass pill, MessageSquare icon, no gradient
- Remove: GitHub star button, Sparkles icon
- Add: `-webkit-app-region: drag`

### DropZone
- Tabs: blue underline active state, not filled background
- Drop area: rounded-lg, dashed `rgba(255,255,255,0.15)` border
- Icons: 48px glass circles, not 80px gradient squares
- Remove: animate-breathe, shadow-glow, scale-105 on drag

### StatusBar
- Minimal: status text + stats, no pulsing dots
- Glass background with subtle top border

### RightPanel
- Tab bar: underline indicator, glass background
- Chat: MessageSquare icon, not Sparkles
- Remove: "NEW" badges, gradient text

### AgentPanel
- Glass inputs, system blue connect button
- Event feed: monospace, subtle tool color tinting
- Remove: bright red/green, replace with muted system colors
