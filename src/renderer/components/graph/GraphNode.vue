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
    json: '{ }',
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
  padding: 7px 11px;
  background: var(--bg-secondary);
  backdrop-filter: blur(30px) saturate(160%);
  -webkit-backdrop-filter: blur(30px) saturate(160%);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  font-size: 12px;
  transition: border-color 0.2s, box-shadow 0.25s, transform 0.15s;
  cursor: default;
}

.graph-node::before {
  content: '';
  position: absolute;
  inset: 0;
  border-radius: inherit;
  background: linear-gradient(
    135deg,
    rgba(255, 255, 255, 0.04) 0%,
    rgba(255, 255, 255, 0.00) 60%
  );
  pointer-events: none;
}

.graph-node:hover {
  border-color: var(--border-light);
  transform: translateY(-1px);
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.25);
}

.graph-node__icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  border-radius: 5px;
  font-size: 9px;
  font-weight: 600;
  letter-spacing: 0.02em;
  flex-shrink: 0;
}

.graph-node__label {
  color: var(--text-primary);
  font-weight: 450;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  max-width: 130px;
}

/* File-type icon tints - warm, desaturated hues */
.graph-node--markdown .graph-node__icon {
  background: rgba(160, 180, 200, 0.12);
  color: rgba(180, 200, 220, 0.80);
}

.graph-node--typescript .graph-node__icon {
  background: rgba(130, 170, 210, 0.12);
  color: rgba(150, 185, 220, 0.80);
}

.graph-node--javascript .graph-node__icon {
  background: rgba(210, 190, 120, 0.12);
  color: rgba(220, 200, 140, 0.80);
}

.graph-node--json .graph-node__icon {
  background: rgba(200, 175, 130, 0.12);
  color: rgba(210, 185, 145, 0.80);
}

.graph-node--yaml .graph-node__icon {
  background: rgba(170, 160, 190, 0.12);
  color: rgba(185, 175, 205, 0.80);
}

.graph-node--directory .graph-node__icon {
  background: rgba(170, 170, 180, 0.10);
  color: rgba(190, 190, 200, 0.70);
}

.graph-node--file .graph-node__icon {
  background: rgba(170, 170, 180, 0.08);
  color: rgba(190, 190, 200, 0.60);
}

/* Activity states */
.graph-node--active {
  border-color: var(--border-light);
  box-shadow: 0 0 10px var(--accent-glow);
}

.graph-node--reading {
  border-color: var(--signal-read);
  box-shadow:
    0 0 0 1px rgba(110, 201, 158, 0.10),
    0 0 16px var(--signal-read-glow);
}

.graph-node--writing {
  border-color: var(--signal-write);
  box-shadow:
    0 0 0 1px rgba(212, 151, 106, 0.10),
    0 0 16px var(--signal-write-glow);
}

.graph-node__pulse {
  position: absolute;
  inset: -3px;
  border-radius: 12px;
  border: 1px solid var(--border-light);
  animation: pulse 1.4s ease-out infinite;
  pointer-events: none;
}

.graph-node--reading .graph-node__pulse {
  border-color: var(--signal-read);
}

.graph-node--writing .graph-node__pulse {
  border-color: var(--signal-write);
}

@keyframes pulse {
  0% {
    opacity: 0.6;
    transform: scale(1);
  }
  100% {
    opacity: 0;
    transform: scale(1.12);
  }
}

:deep(.vue-flow__handle) {
  width: 5px;
  height: 5px;
  background: var(--border);
  border: none;
  opacity: 0;
  transition: opacity 0.15s;
}

.graph-node:hover :deep(.vue-flow__handle) {
  opacity: 1;
}
</style>
