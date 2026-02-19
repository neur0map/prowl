<script setup lang="ts">
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { useGraphStore } from '../../stores/graph'

interface Props {
  id: string
  data: {
    label: string
    path: string
    extension?: string
  }
  type: string
}

const props = defineProps<Props>()
const graphStore = useGraphStore()

const isActive = computed(() => graphStore.activeNodes.includes(props.id))

const activityType = computed(() => {
  const activity = graphStore.activity.get(props.id)
  return activity?.type || 'idle'
})

const nodeClass = computed(() => {
  return [
    'graph-node',
    `graph-node--${props.type}`,
    {
      'graph-node--active': isActive.value,
      'graph-node--reading': activityType.value === 'read',
      'graph-node--writing': activityType.value === 'write'
    }
  ]
})

const icon = computed(() => {
  const icons: Record<string, string> = {
    markdown: 'M',
    typescript: 'TS',
    javascript: 'JS',
    json: '{}',
    yaml: 'Y',
    directory: '/',
    file: 'F'
  }
  return icons[props.type] || 'F'
})
</script>

<template>
  <div :class="nodeClass">
    <Handle type="target" :position="Position.Top" />
    
    <div class="graph-node__icon">{{ icon }}</div>
    <div class="graph-node__label">{{ data.label }}</div>
    
    <div v-if="isActive" class="graph-node__pulse" />
    
    <Handle type="source" :position="Position.Bottom" />
  </div>
</template>

<style scoped>
.graph-node {
  position: relative;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  background: var(--bg-secondary);
  border: 1px solid var(--border);
  border-radius: 8px;
  font-size: 13px;
  transition: all 0.2s ease;
  cursor: pointer;
}

.graph-node:hover {
  border-color: var(--accent);
  transform: translateY(-1px);
}

.graph-node__icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  background: var(--bg-tertiary);
  border-radius: 4px;
  font-size: 10px;
  font-weight: 600;
  color: var(--text-secondary);
}

.graph-node__label {
  color: var(--text-primary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 150px;
}

.graph-node--markdown .graph-node__icon {
  background: rgba(59, 130, 246, 0.2);
  color: rgb(59, 130, 246);
}

.graph-node--typescript .graph-node__icon {
  background: rgba(49, 120, 198, 0.2);
  color: rgb(49, 120, 198);
}

.graph-node--javascript .graph-node__icon {
  background: rgba(247, 223, 30, 0.2);
  color: rgb(247, 223, 30);
}

.graph-node--json .graph-node__icon {
  background: rgba(251, 191, 36, 0.2);
  color: rgb(251, 191, 36);
}

.graph-node--directory .graph-node__icon {
  background: rgba(139, 92, 246, 0.2);
  color: rgb(139, 92, 246);
}

.graph-node--active {
  border-color: var(--accent);
  box-shadow: 0 0 20px var(--accent-glow);
}

.graph-node--reading {
  border-color: rgb(34, 197, 94);
  box-shadow: 0 0 20px rgba(34, 197, 94, 0.4);
}

.graph-node--writing {
  border-color: rgb(249, 115, 22);
  box-shadow: 0 0 20px rgba(249, 115, 22, 0.4);
}

.graph-node__pulse {
  position: absolute;
  inset: -4px;
  border-radius: 12px;
  border: 2px solid var(--accent);
  animation: pulse 1s ease-out infinite;
  pointer-events: none;
}

@keyframes pulse {
  0% {
    opacity: 1;
    transform: scale(1);
  }
  100% {
    opacity: 0;
    transform: scale(1.2);
  }
}

:deep(.vue-flow__handle) {
  width: 8px;
  height: 8px;
  background: var(--accent-dim);
  border: none;
}

:deep(.vue-flow__handle:hover) {
  background: var(--accent);
}
</style>
