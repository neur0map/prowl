<script setup lang="ts">
import { computed } from 'vue'
import { useGraphStore } from '../../stores/graph'

const graphStore = useGraphStore()

const status = computed(() => {
  if (!graphStore.workspacePath) return 'Disconnected'
  return graphStore.workspacePath
})

const isConnected = computed(() => !!graphStore.workspacePath)

const toolStatus = computed(() => {
  if (!graphStore.activeTool) return null
  return graphStore.activeTool
})
</script>

<template>
  <footer class="status-bar">
    <div class="status-bar__left">
      <span
        class="status-bar__dot"
        :class="{ 'status-bar__dot--on': isConnected }"
      />
      <span class="status-bar__text">{{ status }}</span>
    </div>
    <div v-if="toolStatus" class="status-bar__pill">
      {{ toolStatus }}
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
  padding: 0 16px;
  height: 26px;
  background: var(--glass-thin);
  backdrop-filter: blur(40px) saturate(150%);
  -webkit-backdrop-filter: blur(40px) saturate(150%);
  border-top: 1px solid var(--border-inner);
  font-size: 11px;
  user-select: none;
}

.status-bar__left,
.status-bar__right {
  display: flex;
  align-items: center;
  gap: 6px;
}

.status-bar__dot {
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--text-tertiary);
  flex-shrink: 0;
  transition: background 0.3s, box-shadow 0.3s;
}

.status-bar__dot--on {
  background: var(--signal-read);
  box-shadow: 0 0 4px var(--signal-read-glow);
}

.status-bar__text {
  color: var(--text-tertiary);
  font-weight: 400;
}

.status-bar__pill {
  padding: 1px 7px;
  background: var(--glass-medium);
  border: 1px solid var(--border);
  border-radius: 4px;
  color: var(--text-secondary);
  font-weight: 500;
  font-size: 10px;
  animation: pill-in 0.15s ease-out;
}

@keyframes pill-in {
  from { opacity: 0; transform: scale(0.94); }
  to   { opacity: 1; transform: scale(1); }
}
</style>
