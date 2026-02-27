/**
 * Renderer-side MCP tool dispatcher.
 *
 * Receives tool name + params from the main process via IPC,
 * executes against the Comlink worker proxy, and returns
 * a JSON-serialisable result.
 */

import type { Remote } from 'comlink';
import type { IndexerWorkerApi } from '../workers/ingestion.worker';
import type {
  McpToolName,
  McpToolRequest,
  McpToolResponse,
  SearchParams,
  CypherParams,
  GrepParams,
  ReadFileParams,
  ExploreParams,
  ImpactParams,
  GetContextParams,
  GetHotspotsParams,
  AskParams,
  InvestigateParams,
} from './types';

type WorkerApi = Remote<IndexerWorkerApi>;

let recentQueries: Array<{ tool: string; ts: number }> = [];
const MAX_RECENT = 20;

function trackQuery(tool: string): void {
  recentQueries.push({ tool, ts: Date.now() });
  if (recentQueries.length > MAX_RECENT) {
    recentQueries = recentQueries.slice(-MAX_RECENT);
  }
}

export function getRecentQueries(): Array<{ tool: string; ts: number }> {
  return recentQueries.slice(-5);
}

export async function executeMcpTool(
  api: WorkerApi,
  toolName: McpToolName,
  params: unknown,
  extra?: {
    projectName?: string;
    chatMessages?: Array<{ role: string; content: string }>;
  },
): Promise<McpToolResponse> {
  const requestId = '';
  try {
    trackQuery(toolName);
    const result = await dispatch(api, toolName, params, extra);
    return { requestId, success: true, data: result };
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    return { requestId, success: false, error: message };
  }
}

async function dispatch(
  api: WorkerApi,
  toolName: McpToolName,
  params: unknown,
  extra?: {
    projectName?: string;
    chatMessages?: Array<{ role: string; content: string }>;
  },
): Promise<unknown> {
  switch (toolName) {
    case 'status': {
      const ready = await api.isReady();
      const stats = ready ? await api.getStats() : { nodes: 0, edges: 0 };
      const agentReady = await api.isAgentReady();
      return {
        ready,
        projectName: extra?.projectName || 'unknown',
        nodes: stats.nodes,
        edges: stats.edges,
        agentReady,
      };
    }

    case 'search': {
      const p = params as SearchParams;
      return api.hybridSearch(p.query, p.limit ?? 10, p.useReranker);
    }

    case 'cypher': {
      const p = params as CypherParams;
      return api.runQuery(p.cypher);
    }

    case 'grep': {
      const p = params as GrepParams;
      return api.mcpGrep(p.pattern, p.fileFilter, p.caseSensitive, p.maxResults);
    }

    case 'read-file': {
      const p = params as ReadFileParams;
      return api.mcpReadFile(p.filePath);
    }

    case 'overview': {
      const queries = await Promise.all([
        api.runQuery(`
          MATCH (c:Community)
          RETURN c.id AS id, c.label AS label, c.cohesion AS cohesion,
                 c.symbolCount AS symbolCount, c.description AS description
          ORDER BY c.symbolCount DESC LIMIT 200
        `),
        api.runQuery(`
          MATCH (p:Process)
          RETURN p.id AS id, p.label AS label, p.processType AS type,
                 p.stepCount AS stepCount, p.communities AS communities
          ORDER BY p.stepCount DESC LIMIT 200
        `),
      ]);
      return { clusters: queries[0], processes: queries[1] };
    }

    case 'explore': {
      const p = params as ExploreParams;
      const escaped = p.target.replace(/'/g, "''");

      if (!p.type || p.type === 'process') {
        const rows = await api.runQuery(`
          MATCH (proc:Process) WHERE proc.id = '${escaped}' OR proc.label = '${escaped}'
          RETURN proc.id AS id, proc.label AS label, proc.processType AS type, proc.stepCount AS stepCount
          LIMIT 1
        `);
        if (rows.length > 0) {
          const pid = rows[0].id || rows[0][0];
          const escapedPid = String(pid).replace(/'/g, "''");
          const steps = await api.runQuery(`
            MATCH (s)-[r:CodeEdge {type: 'STEP_IN_PROCESS'}]->(p:Process {id: '${escapedPid}'})
            RETURN s.name AS name, s.filePath AS filePath, r.step AS step
            ORDER BY r.step
          `);
          return { kind: 'process', info: rows[0], steps };
        }
      }

      if (!p.type || p.type === 'cluster') {
        const rows = await api.runQuery(`
          MATCH (c:Community)
          WHERE c.id = '${escaped}' OR c.label = '${escaped}' OR c.heuristicLabel = '${escaped}'
          RETURN c.id AS id, c.label AS label, c.cohesion AS cohesion,
                 c.symbolCount AS symbolCount, c.description AS description
          LIMIT 1
        `);
        if (rows.length > 0) {
          const cid = rows[0].id || rows[0][0];
          const escapedCid = String(cid).replace(/'/g, "''");
          const members = await api.runQuery(`
            MATCH (c:Community {id: '${escapedCid}'})<-[:CodeEdge {type: 'MEMBER_OF'}]-(m)
            RETURN m.name AS name, m.filePath AS filePath, label(m) AS nodeType
            LIMIT 50
          `);
          return { kind: 'cluster', info: rows[0], members };
        }
      }

      if (!p.type || p.type === 'symbol') {
        let rows: any[] = [];

        // Auto-detect file paths (contains / or .)
        if (p.target.includes('/') || p.target.includes('.')) {
          rows = await api.runQuery(`
            MATCH (n) WHERE n.filePath IS NOT NULL
              AND (n.filePath = '${escaped}' OR n.filePath CONTAINS '${escaped}')
            RETURN n.id AS id, n.name AS name, n.filePath AS filePath, label(n) AS nodeType
            LIMIT 10
          `);
        }

        // Fall back to name/id search
        if (rows.length === 0) {
          rows = await api.runQuery(`
            MATCH (n) WHERE n.name = '${escaped}' OR n.id = '${escaped}'
            RETURN n.id AS id, n.name AS name, n.filePath AS filePath, label(n) AS nodeType
            LIMIT 5
          `);
        }

        if (rows.length > 0) {
          return { kind: 'symbol', results: rows };
        }
      }

      return { kind: 'not_found', target: p.target };
    }

    case 'impact': {
      const p = params as ImpactParams;
      const escaped = p.target.replace(/'/g, "''");
      const dir = p.direction || 'upstream';
      const relTypes = (p.relationTypes?.length ? p.relationTypes : ['CALLS', 'IMPORTS', 'EXTENDS', 'IMPLEMENTS'])
        .map(t => `'${t}'`).join(', ');

      // Step 1: Find target node(s)
      // For file paths: find File AND all symbols defined in it
      let targetIds: string[] = [];

      if (p.target.includes('/')) {
        const fileNodes = await api.runQuery(`
          MATCH (n) WHERE n.filePath IS NOT NULL AND n.filePath CONTAINS '${escaped}'
          RETURN n.id AS id LIMIT 50
        `);
        targetIds = fileNodes.map((r: any) => r.id);
      } else {
        const symbolNodes = await api.runQuery(`
          MATCH (n) WHERE n.name = '${escaped}'
          RETURN n.id AS id LIMIT 10
        `);
        targetIds = symbolNodes.map((r: any) => r.id);
      }

      if (targetIds.length === 0) {
        return { error: `Could not find "${p.target}" in the codebase.` };
      }

      // Step 2: Query impact for ALL matched nodes
      const idList = targetIds.map(id => `'${String(id).replace(/'/g, "''")}'`).join(', ');

      const query = dir === 'upstream'
        ? `MATCH (affected)-[r:CodeEdge]->(target)
           WHERE target.id IN [${idList}] AND r.type IN [${relTypes}]
           RETURN DISTINCT affected.id AS id, affected.name AS name,
                  label(affected) AS nodeType, affected.filePath AS filePath, r.type AS edgeType
           LIMIT 100`
        : `MATCH (target)-[r:CodeEdge]->(affected)
           WHERE target.id IN [${idList}] AND r.type IN [${relTypes}]
           RETURN DISTINCT affected.id AS id, affected.name AS name,
                  label(affected) AS nodeType, affected.filePath AS filePath, r.type AS edgeType
           LIMIT 100`;

      const affected = await api.runQuery(query);
      return { target: p.target, direction: dir, affected };
    }

    case 'get-context': {
      const p = params as GetContextParams;
      return api.mcpGetContext(p.projectName);
    }

    case 'get-hotspots': {
      const p = params as GetHotspotsParams;
      return api.mcpGetHotspots(p.limit);
    }

    case 'chat-history': {
      return extra?.chatMessages ?? [];
    }

    case 'ask': {
      const p = params as AskParams;
      return { response: await api.mcpAsk(p.question) };
    }

    case 'investigate': {
      const p = params as InvestigateParams;
      return { response: await api.mcpInvestigate(p.task, p.depth) };
    }

    default:
      throw new Error(`Unknown MCP tool: ${toolName}`);
  }
}
