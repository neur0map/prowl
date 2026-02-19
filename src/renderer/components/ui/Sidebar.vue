<script setup lang="ts">
import { ref } from 'vue'
import { useGraphStore } from '../../stores/graph'

const graphStore = useGraphStore()
const workspacePath = ref('')
const logPath = ref('')

async function handleConnect(): Promise<void> {
  if (!workspacePath.value) return
  await graphStore.loadWorkspace(workspacePath.value)
}

async function handleWatchLogs(): Promise<void> {
  if (!logPath.value) return
  await window.api.watchLogs(logPath.value)
}

async function handleBrowseWorkspace(): Promise<void> {
  const selected = await window.api.selectDirectory()
  if (selected) {
    workspacePath.value = selected
  }
}

async function handleBrowseLogFile(): Promise<void> {
  const selected = await window.api.selectFile()
  if (selected) {
    logPath.value = selected
  }
}
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar__drag-region" />

    <header class="sidebar__header">
      <h1 class="sidebar__title">OpenClaw Viz</h1>
    </header>

    <div class="sidebar__body">
      <section class="sidebar__section">
        <h2 class="sidebar__label">Workspace</h2>
        <div class="sidebar__field">
          <div class="sidebar__input-row">
            <input
              v-model="workspacePath"
              type="text"
              placeholder="/path/to/workspace"
              class="sidebar__input"
            />
            <button class="sidebar__browse" @click="handleBrowseWorkspace" title="Browse">
              <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
                <path d="M2 4a2 2 0 0 1 2-2h2.6l1.4 2h4a2 2 0 0 1 2 2v6a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4Z" stroke="currentColor" stroke-width="1.2"/>
              </svg>
            </button>
          </div>
          <button class="sidebar__action" @click="handleConnect">
            Connect
          </button>
        </div>
      </section>

      <section class="sidebar__section">
        <h2 class="sidebar__label">Log File</h2>
        <div class="sidebar__field">
          <div class="sidebar__input-row">
            <input
              v-model="logPath"
              type="text"
              placeholder="/path/to/logfile.log"
              class="sidebar__input"
            />
            <button class="sidebar__browse" @click="handleBrowseLogFile" title="Browse">
              <svg width="13" height="13" viewBox="0 0 16 16" fill="none">
                <path d="M4 2h6l3 3v8a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2Z" stroke="currentColor" stroke-width="1.2"/>
                <path d="M10 2v3h3" stroke="currentColor" stroke-width="1.2"/>
              </svg>
            </button>
          </div>
          <button class="sidebar__action" @click="handleWatchLogs">
            Watch
          </button>
        </div>
      </section>

      <div class="sidebar__separator" />

      <section class="sidebar__section">
        <h2 class="sidebar__label">Legend</h2>
        <ul class="sidebar__legend">
          <li class="sidebar__legend-item">
            <span class="sidebar__dot sidebar__dot--idle"></span>
            <span>Idle</span>
          </li>
          <li class="sidebar__legend-item">
            <span class="sidebar__dot sidebar__dot--read"></span>
            <span>Reading</span>
          </li>
          <li class="sidebar__legend-item">
            <span class="sidebar__dot sidebar__dot--write"></span>
            <span>Writing</span>
          </li>
        </ul>
      </section>
    </div>

    <footer class="sidebar__footer">
      <dl class="sidebar__stats">
        <div class="sidebar__stat">
          <dd>{{ graphStore.nodes.length }}</dd>
          <dt>Nodes</dt>
        </div>
        <div class="sidebar__stat">
          <dd>{{ graphStore.edges.length }}</dd>
          <dt>Edges</dt>
        </div>
        <div class="sidebar__stat">
          <dd>{{ graphStore.activeNodes.length }}</dd>
          <dt>Active</dt>
        </div>
      </dl>
    </footer>
  </aside>
</template>

<style scoped>
.sidebar {
  grid-row: 1 / -1;
  display: flex;
  flex-direction: column;
  background: var(--bg-secondary);
  backdrop-filter: blur(60px) saturate(180%);
  -webkit-backdrop-filter: blur(60px) saturate(180%);
  border-right: 1px solid var(--border);
  overflow: hidden;
  user-select: none;
}

.sidebar__drag-region {
  height: 52px;
  -webkit-app-region: drag;
  flex-shrink: 0;
}

.sidebar__header {
  padding: 0 20px 20px;
}

.sidebar__title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-primary);
  letter-spacing: -0.01em;
}

.sidebar__body {
  flex: 1;
  overflow-y: auto;
  padding: 0 16px;
}

.sidebar__section {
  margin-bottom: 18px;
}

.sidebar__label {
  font-size: 11px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  color: var(--text-tertiary);
  margin-bottom: 8px;
  padding: 0 2px;
}

.sidebar__field {
  display: flex;
  flex-direction: column;
  gap: 5px;
}

.sidebar__input-row {
  display: flex;
  gap: 4px;
}

.sidebar__input {
  flex: 1;
  min-width: 0;
  padding: 6px 9px;
  background: var(--glass-thin);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-primary);
  font-size: 12px;
  outline: none;
  transition: border-color 0.15s, background 0.15s;
}

.sidebar__input:focus {
  border-color: var(--border-light);
  background: var(--glass-medium);
}

.sidebar__input::placeholder {
  color: var(--text-tertiary);
}

.sidebar__browse {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  flex-shrink: 0;
  background: var(--glass-thin);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-tertiary);
  cursor: pointer;
  transition: color 0.15s, background 0.15s, border-color 0.15s;
  -webkit-app-region: no-drag;
}

.sidebar__browse:hover {
  color: var(--text-secondary);
  background: var(--glass-medium);
  border-color: var(--border-light);
}

.sidebar__action {
  padding: 6px 0;
  background: var(--glass-thick);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-primary);
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  transition: background 0.15s, border-color 0.15s;
  -webkit-app-region: no-drag;
}

.sidebar__action:hover {
  background: var(--glass-medium);
  border-color: var(--border-light);
}

.sidebar__action:active {
  background: var(--glass-thin);
}

.sidebar__separator {
  height: 1px;
  background: var(--border-inner);
  margin: 2px 0 18px;
}

.sidebar__legend {
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 7px;
  padding: 0 2px;
}

.sidebar__legend-item {
  display: flex;
  align-items: center;
  gap: 9px;
  font-size: 12px;
  color: var(--text-secondary);
}

.sidebar__dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  flex-shrink: 0;
}

.sidebar__dot--idle {
  background: var(--text-tertiary);
}

.sidebar__dot--read {
  background: var(--signal-read);
  box-shadow: 0 0 5px var(--signal-read-glow);
}

.sidebar__dot--write {
  background: var(--signal-write);
  box-shadow: 0 0 5px var(--signal-write-glow);
}

.sidebar__footer {
  padding: 12px 16px;
  border-top: 1px solid var(--border-inner);
  background: var(--glass-thin);
}

.sidebar__stats {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 8px;
  margin: 0;
}

.sidebar__stat {
  text-align: center;
}

.sidebar__stat dd {
  font-size: 17px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--text-primary);
  margin: 0 0 1px;
  line-height: 1.2;
}

.sidebar__stat dt {
  font-size: 10px;
  font-weight: 500;
  text-transform: uppercase;
  letter-spacing: 0.03em;
  color: var(--text-tertiary);
}
</style>
