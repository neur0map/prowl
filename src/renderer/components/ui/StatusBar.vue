<script setup lang="ts">
import { computed } from 'vue'
import { useGraphStore } from '../../stores/graph'

const graphStore = useGraphStore()

const status = computed(() => {
  if (!graphStore.workspacePath) return 'Disconnected'
  return `Watching: ${graphStore.workspacePath}`
})

const toolStatus = computed(() => {
  if (!graphStore.activeTool) return null
  return graphStore.activeTool
})
</script>

<template>
  <footer class="status-bar">
    <div class="status-bar__left">
      <span class="status-bar__indicator" :class="{ 'status-bar__indicator--active': graphStore.workspacePath }"></span>
      <span class="status-bar__text">{{ status }}</span>
    </div>
    <div v-if="toolStatus" class="status-bar__tool">
      <span class="status-bar__tool-label">Tool:</span>
      <span class="status-bar__tool-name">{{ toolStatus }}</span>
    </div>
    <div class="status-bar__right">
      <span class="status-bar__text">v0.1.0</span>
    </div>
  </footer>
</template>

<style scoped>
.status-bar {
  grid-column: 1 / -1;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 16px;
  background: var(--bg-tertiary);
  border-top: 1px solid var(--border);
  font-size: 12px;
}

.status-bar__left,
.status-bar__right {
  display: flex;
  align-items: center;
  gap: 8px;
}

.status-bar__indicator {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--text-tertiary);
}

.status-bar__indicator--active {
  background: rgb(34, 197, 94);
  box-shadow: 0 0 6px rgba(34, 197, 94, 0.6);
}

.status-bar__text {
  color: var(--text-secondary);
}

.status-bar__tool {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 2px 10px;
  background: var(--accent);
  border-radius: 4px;
  animation: tool-pulse 0.5s ease-out;
}

.status-bar__tool-label {
  color: rgba(255, 255, 255, 0.7);
}

.status-bar__tool-name {
  color: white;
  font-weight: 500;
}

@keyframes tool-pulse {
  0% {
    transform: scale(1.1);
  }
  100% {
    transform: scale(1);
  }
}
</style>
