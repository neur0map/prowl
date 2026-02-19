<script setup lang="ts">
import { computed } from 'vue'
import { VueFlow, useVueFlow } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { MiniMap } from '@vue-flow/minimap'
import { useGraphStore } from '../../stores/graph'
import GraphNode from './GraphNode.vue'

const graphStore = useGraphStore()
const { fitView } = useVueFlow()

const nodeTypes = {
  markdown: GraphNode,
  typescript: GraphNode,
  javascript: GraphNode,
  json: GraphNode,
  yaml: GraphNode,
  file: GraphNode,
  directory: GraphNode
}

const nodes = computed(() => graphStore.nodes)
const edges = computed(() => graphStore.edges)
</script>

<template>
  <div class="graph-canvas">
    <VueFlow
      v-model:nodes="nodes"
      v-model:edges="edges"
      :node-types="nodeTypes"
      :default-edge-options="{ type: 'smoothstep', animated: true }"
      :min-zoom="0.1"
      :max-zoom="2"
      fit-view-on-init
      class="flow"
    >
      <Background :gap="20" :size="1" pattern-color="rgba(255, 255, 255, 0.03)" />
      <MiniMap 
        position="bottom-right" 
        :node-color="() => 'rgba(99, 102, 241, 0.6)'"
        :mask-color="'rgba(0, 0, 0, 0.8)'"
      />
    </VueFlow>
  </div>
</template>

<style scoped>
.graph-canvas {
  width: 100%;
  height: 100%;
}

.flow {
  background: var(--bg-primary);
}

.flow :deep(.vue-flow__edge-path) {
  stroke: var(--accent-dim);
  stroke-width: 2;
}

.flow :deep(.vue-flow__edge.animated path) {
  stroke-dasharray: 5;
  animation: dash 0.5s linear infinite;
}

@keyframes dash {
  to {
    stroke-dashoffset: -10;
  }
}
</style>
