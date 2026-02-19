<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'
import { useGraphStore } from './stores/graph'
import GraphCanvas from './components/graph/GraphCanvas.vue'
import Sidebar from './components/ui/Sidebar.vue'
import StatusBar from './components/ui/StatusBar.vue'

const graphStore = useGraphStore()

function mapToolToActivationType(tool: string): 'read' | 'write' {
  return tool === 'write' || tool === 'edit' ? 'write' : 'read'
}

onMounted(() => {
  window.api.onNodeActivate((data) => {
    graphStore.activateNode(data.filepath, data.type)
  })

  window.api.onToolActivate((data) => {
    graphStore.activateTool(data.tool)
    if (data.filepath) {
      graphStore.activateNode(data.filepath, mapToolToActivationType(data.tool))
    }
  })

  window.api.onGraphUpdate((data) => {
    if (data.action === 'add') {
      graphStore.addNode(data.filepath)
    } else {
      graphStore.removeNode(data.filepath)
    }
  })
})

onUnmounted(() => {
  window.api.removeAllListeners()
})
</script>

<template>
  <div class="app">
    <Sidebar />
    <div class="main-area">
      <div class="main-area__drag-region" />
      <main class="main">
        <GraphCanvas />
      </main>
    </div>
    <StatusBar />
  </div>
</template>

<style scoped>
.app {
  display: grid;
  grid-template-columns: 260px 1fr;
  grid-template-rows: 1fr auto;
  height: 100vh;
  background: transparent;
  color: var(--text-primary);
  border-radius: 10px;
  overflow: hidden;
}

.main-area {
  grid-row: 1;
  grid-column: 2;
  position: relative;
  overflow: hidden;
  background: var(--bg-primary);
}

.main-area__drag-region {
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  height: 52px;
  -webkit-app-region: drag;
  z-index: 10;
  pointer-events: auto;
}

.main {
  width: 100%;
  height: 100%;
}
</style>
