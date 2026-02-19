import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import type { Node, Edge } from '@vue-flow/core'
import * as dagre from '@dagrejs/dagre'

const NODE_WIDTH = 180
const NODE_HEIGHT = 44

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
    const { collectedNodes, collectedEdges } = collectNodesAndEdges(fileTree)
    applyDagreLayout(collectedNodes, collectedEdges)
  }

  function collectNodesAndEdges(
    fileTree: any[],
    parentId?: string
  ): { collectedNodes: Node[]; collectedEdges: Edge[] } {
    const collectedNodes: Node[] = []
    const collectedEdges: Edge[] = []

    for (const file of fileTree) {
      const nodeId = file.path

      collectedNodes.push({
        id: nodeId,
        type: file.type === 'directory' ? 'directory' : getNodeType(file.extension),
        position: { x: 0, y: 0 },
        data: {
          label: file.name,
          path: file.path,
          extension: file.extension
        }
      })

      if (parentId) {
        collectedEdges.push({
          id: `${parentId}-${nodeId}`,
          source: parentId,
          target: nodeId,
          type: 'smoothstep'
        })
      }

      if (file.children) {
        const child = collectNodesAndEdges(file.children, nodeId)
        collectedNodes.push(...child.collectedNodes)
        collectedEdges.push(...child.collectedEdges)
      }
    }

    return { collectedNodes, collectedEdges }
  }

  function applyDagreLayout(collectedNodes: Node[], collectedEdges: Edge[]): void {
    const g = new dagre.graphlib.Graph()
    g.setDefaultEdgeLabel(() => ({}))
    g.setGraph({ rankdir: 'TB', ranksep: 120, nodesep: 40 })

    for (const node of collectedNodes) {
      g.setNode(node.id, { width: NODE_WIDTH, height: NODE_HEIGHT })
    }

    for (const edge of collectedEdges) {
      g.setEdge(edge.source, edge.target)
    }

    dagre.layout(g)

    for (const node of collectedNodes) {
      const dagreNode = g.node(node.id)
      // dagre gives center coords; convert to top-left for Vue Flow
      node.position = {
        x: dagreNode.x - NODE_WIDTH / 2,
        y: dagreNode.y - NODE_HEIGHT / 2
      }
    }

    nodes.value = collectedNodes
    edges.value = collectedEdges
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
