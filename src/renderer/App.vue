<script setup lang="ts">
import { onMounted, onUnmounted } from 'vue'
import { useGraphStore } from './stores/graph'
import GraphCanvas from './components/graph/GraphCanvas.vue'
import Sidebar from './components/ui/Sidebar.vue'
import StatusBar from './components/ui/StatusBar.vue'

const graphStore = useGraphStore()

onMounted(() => {
  window.api.onNodeActivate((data) => {
    graphStore.activateNode(data.filepath, data.type)
  })

  window.api.onToolActivate((data) => {
    graphStore.activateTool(data.tool)
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
    <main class="main">
      <GraphCanvas />
    </main>
    <StatusBar />
  </div>
</template>

<style scoped>
.app {
  display: grid;
  grid-template-columns: 280px 1fr;
  grid-template-rows: 1fr auto;
  height: 100vh;
  background: var(--bg-primary);
  color: var(--text-primary);
}

.main {
  grid-row: 1;
  grid-column: 2;
  position: relative;
  overflow: hidden;
}
</style>
