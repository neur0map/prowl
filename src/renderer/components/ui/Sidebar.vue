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
</script>

<template>
  <aside class="sidebar">
    <header class="sidebar__header">
      <h1 class="sidebar__title">OpenClaw Viz</h1>
    </header>

    <section class="sidebar__section">
      <h2 class="sidebar__section-title">Workspace</h2>
      <div class="sidebar__input-group">
        <input
          v-model="workspacePath"
          type="text"
          placeholder="~/.openclaw/workspace"
          class="sidebar__input"
        />
        <button class="sidebar__button" @click="handleConnect">
          Connect
        </button>
      </div>
    </section>

    <section class="sidebar__section">
      <h2 class="sidebar__section-title">Log File</h2>
      <div class="sidebar__input-group">
        <input
          v-model="logPath"
          type="text"
          placeholder="~/.openclaw/logs/main.log"
          class="sidebar__input"
        />
        <button class="sidebar__button" @click="handleWatchLogs">
          Watch
        </button>
      </div>
    </section>

    <section class="sidebar__section">
      <h2 class="sidebar__section-title">Legend</h2>
      <ul class="sidebar__legend">
        <li class="sidebar__legend-item">
          <span class="sidebar__legend-color sidebar__legend-color--idle"></span>
          <span>Idle</span>
        </li>
        <li class="sidebar__legend-item">
          <span class="sidebar__legend-color sidebar__legend-color--read"></span>
          <span>Reading</span>
        </li>
        <li class="sidebar__legend-item">
          <span class="sidebar__legend-color sidebar__legend-color--write"></span>
          <span>Writing</span>
        </li>
      </ul>
    </section>

    <section class="sidebar__section sidebar__section--stats">
      <h2 class="sidebar__section-title">Stats</h2>
      <dl class="sidebar__stats">
        <div class="sidebar__stat">
          <dt>Nodes</dt>
          <dd>{{ graphStore.nodes.length }}</dd>
        </div>
        <div class="sidebar__stat">
          <dt>Edges</dt>
          <dd>{{ graphStore.edges.length }}</dd>
        </div>
        <div class="sidebar__stat">
          <dt>Active</dt>
          <dd>{{ graphStore.activeNodes.length }}</dd>
        </div>
      </dl>
    </section>
  </aside>
</template>

<style scoped>
.sidebar {
  grid-row: 1 / -1;
  display: flex;
  flex-direction: column;
  background: var(--bg-secondary);
  border-right: 1px solid var(--border);
  overflow-y: auto;
}

.sidebar__header {
  padding: 20px;
  border-bottom: 1px solid var(--border);
}

.sidebar__title {
  font-size: 16px;
  font-weight: 600;
  color: var(--text-primary);
  margin: 0;
}

.sidebar__section {
  padding: 16px 20px;
  border-bottom: 1px solid var(--border);
}

.sidebar__section-title {
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--text-tertiary);
  margin: 0 0 12px;
}

.sidebar__input-group {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.sidebar__input {
  padding: 8px 12px;
  background: var(--bg-tertiary);
  border: 1px solid var(--border);
  border-radius: 6px;
  color: var(--text-primary);
  font-size: 13px;
}

.sidebar__input:focus {
  outline: none;
  border-color: var(--accent);
}

.sidebar__input::placeholder {
  color: var(--text-tertiary);
}

.sidebar__button {
  padding: 8px 16px;
  background: var(--accent);
  border: none;
  border-radius: 6px;
  color: white;
  font-size: 13px;
  font-weight: 500;
  cursor: pointer;
  transition: opacity 0.2s;
}

.sidebar__button:hover {
  opacity: 0.9;
}

.sidebar__legend {
  list-style: none;
  padding: 0;
  margin: 0;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.sidebar__legend-item {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 13px;
  color: var(--text-secondary);
}

.sidebar__legend-color {
  width: 12px;
  height: 12px;
  border-radius: 3px;
}

.sidebar__legend-color--idle {
  background: var(--bg-tertiary);
  border: 1px solid var(--border);
}

.sidebar__legend-color--read {
  background: rgb(34, 197, 94);
  box-shadow: 0 0 8px rgba(34, 197, 94, 0.5);
}

.sidebar__legend-color--write {
  background: rgb(249, 115, 22);
  box-shadow: 0 0 8px rgba(249, 115, 22, 0.5);
}

.sidebar__section--stats {
  margin-top: auto;
}

.sidebar__stats {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 12px;
  margin: 0;
}

.sidebar__stat {
  text-align: center;
}

.sidebar__stat dt {
  font-size: 11px;
  color: var(--text-tertiary);
  margin-bottom: 4px;
}

.sidebar__stat dd {
  font-size: 20px;
  font-weight: 600;
  color: var(--text-primary);
  margin: 0;
}
</style>
