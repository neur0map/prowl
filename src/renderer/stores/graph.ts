import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import type { Node, Edge } from '@vue-flow/core'

interface NodeActivity {
  lastActive: number
  type: 'read' | 'write' | 'idle'
}

export const useGraphStore = defineStore('graph', () => {
  const nodes = ref<Node[]>([])
  const edges = ref<Edge[]>([])
  const activity = ref<Map<string, NodeActivity>>(new Map())
  const workspacePath = ref<string>('')
  const activeTool = ref<string | null>(null)

  const activeNodes = computed(() => {
    const now = Date.now()
    const activeIds: string[] = []
    
    activity.value.forEach((data, id) => {
      if (now - data.lastActive < 2000) {
        activeIds.push(id)
      }
    })
    
    return activeIds
  })

  async function loadWorkspace(path: string): Promise<void> {
    workspacePath.value = path
    const fileTree = await window.api.scanWorkspace(path)
    buildGraph(fileTree)
  }

  function buildGraph(fileTree: any[], parentId?: string, depth = 0): void {
    const newNodes: Node[] = []
    const newEdges: Edge[] = []
    const positions = calculatePositions(fileTree.length, depth)

    fileTree.forEach((file, index) => {
      const nodeId = file.path
      const position = positions[index]

      newNodes.push({
        id: nodeId,
        type: file.type === 'directory' ? 'directory' : getNodeType(file.extension),
        position,
        data: {
          label: file.name,
          path: file.path,
          extension: file.extension
        }
      })

      if (parentId) {
        newEdges.push({
          id: `${parentId}-${nodeId}`,
          source: parentId,
          target: nodeId,
          type: 'smoothstep'
        })
      }

      if (file.children) {
        const childGraph = buildChildGraph(file.children, nodeId, depth + 1)
        newNodes.push(...childGraph.nodes)
        newEdges.push(...childGraph.edges)
      }
    })

    nodes.value = newNodes
    edges.value = newEdges
  }

  function buildChildGraph(children: any[], parentId: string, depth: number): { nodes: Node[], edges: Edge[] } {
    const result = { nodes: [] as Node[], edges: [] as Edge[] }
    const positions = calculatePositions(children.length, depth)

    children.forEach((file, index) => {
      const nodeId = file.path
      const position = positions[index]

      result.nodes.push({
        id: nodeId,
        type: file.type === 'directory' ? 'directory' : getNodeType(file.extension),
        position,
        data: {
          label: file.name,
          path: file.path,
          extension: file.extension
        }
      })

      result.edges.push({
        id: `${parentId}-${nodeId}`,
        source: parentId,
        target: nodeId,
        type: 'smoothstep'
      })

      if (file.children) {
        const childGraph = buildChildGraph(file.children, nodeId, depth + 1)
        result.nodes.push(...childGraph.nodes)
        result.edges.push(...childGraph.edges)
      }
    })

    return result
  }

  function calculatePositions(count: number, depth: number): { x: number, y: number }[] {
    const positions: { x: number, y: number }[] = []
    const centerX = 400
    const startY = depth * 150
    const spacing = 200

    const startX = centerX - ((count - 1) * spacing) / 2

    for (let i = 0; i < count; i++) {
      positions.push({
        x: startX + i * spacing,
        y: startY
      })
    }

    return positions
  }

  function getNodeType(extension?: string): string {
    const types: Record<string, string> = {
      '.md': 'markdown',
      '.ts': 'typescript',
      '.js': 'javascript',
      '.json': 'json',
      '.yaml': 'yaml',
      '.yml': 'yaml'
    }
    return types[extension || ''] || 'file'
  }

  function activateNode(filepath: string, type: 'read' | 'write'): void {
    activity.value.set(filepath, {
      lastActive: Date.now(),
      type
    })

    setTimeout(() => {
      const current = activity.value.get(filepath)
      if (current && Date.now() - current.lastActive >= 2000) {
        activity.value.delete(filepath)
      }
    }, 2500)
  }

  function activateTool(tool: string): void {
    activeTool.value = tool
    setTimeout(() => {
      if (activeTool.value === tool) {
        activeTool.value = null
      }
    }, 1500)
  }

  function addNode(filepath: string): void {
    const parts = filepath.split('/')
    const name = parts[parts.length - 1]
    const extension = name.includes('.') ? '.' + name.split('.').pop() : undefined

    nodes.value.push({
      id: filepath,
      type: getNodeType(extension),
      position: { x: Math.random() * 600, y: Math.random() * 400 },
      data: {
        label: name,
        path: filepath,
        extension
      }
    })
  }

  function removeNode(filepath: string): void {
    nodes.value = nodes.value.filter(n => n.id !== filepath)
    edges.value = edges.value.filter(e => e.source !== filepath && e.target !== filepath)
  }

  return {
    nodes,
    edges,
    activity,
    workspacePath,
    activeTool,
    activeNodes,
    loadWorkspace,
    activateNode,
    activateTool,
    addNode,
    removeNode
  }
})
