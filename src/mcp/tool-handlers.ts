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
  CompareParams,
  CompareFileTreeParams,
  CompareReadFileParams,
  CompareGrepParams,
  DetectChangesParams,
} from './types';
import { parseGitHubUrl } from '../services/git-clone';

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
    projectPath?: string;
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
    projectPath?: string;
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

    case 'compare': {
      const p = params as CompareParams;
      const parsed = parseGitHubUrl(p.repo_url);
      if (!parsed) {
        throw new Error('Invalid GitHub URL. Expected format: https://github.com/owner/repo');
      }
      const { owner, repo } = parsed;

      // Only one comparison at a time
      const existingStats = await api.getComparisonStats();
      if (existingStats) {
        if (existingStats.repoUrl === p.repo_url) {
          return {
            message: 'Comparison project already loaded.',
            stats: existingStats,
          };
        }
        throw new Error(
          `A comparison project is already loaded ("${existingStats.repoName}"). ` +
          'Close it first with prowl_compare_file_tree or the UI before loading another.'
        );
      }

      // Fetch repo info + tree via GitHub REST API
      const repoInfo = await window.prowl.github.getRepoInfo(owner, repo, p.token);
      const branch = p.branch || repoInfo.defaultBranch;
      const { entries, truncated } = await window.prowl.github.getRepoTree(owner, repo, branch, p.token);

      const meta = {
        owner,
        repo,
        branch,
        repoName: repoInfo.fullName,
        repoUrl: p.repo_url,
        description: repoInfo.description,
        token: p.token,
      };

      api.loadComparison(meta, entries);
      const stats = await api.getComparisonStats();
      const truncNote = truncated ? ' (tree truncated — very large repo)' : '';
      return {
        message: `Loaded comparison project "${repoInfo.fullName}" (${stats?.fileCount ?? 0} files, ${stats?.dirCount ?? 0} dirs).${truncNote}\nUse prowl_compare_file_tree to browse, prowl_compare_read_file to read files.`,
        stats,
      };
    }

    case 'compare-file-tree': {
      const p = params as CompareFileTreeParams;
      const loaded = await api.isComparisonLoaded();
      if (!loaded) throw new Error('No comparison project loaded. Use prowl_compare first.');
      const entries = await api.getComparisonTree(p.dir_path);
      return {
        dir_path: p.dir_path || '/',
        entries: entries.map(e => ({
          path: e.path,
          type: e.type,
          size: e.size,
        })),
        count: entries.length,
      };
    }

    case 'compare-read-file': {
      const p = params as CompareReadFileParams;
      const loaded = await api.isComparisonLoaded();
      if (!loaded) throw new Error('No comparison project loaded. Use prowl_compare first.');

      // Check cache first
      const cached = await api.getComparisonFile(p.file_path);
      if (cached !== null) {
        return { file_path: p.file_path, content: cached, cached: true };
      }

      // Fetch from GitHub
      const meta = await api.getComparisonMeta();
      if (!meta) throw new Error('Comparison metadata not available.');
      const content = await window.prowl.github.readFile(
        meta.owner, meta.repo, meta.branch, p.file_path, meta.token,
      );
      api.cacheComparisonFile(p.file_path, content);
      return { file_path: p.file_path, content, cached: false };
    }

    case 'compare-grep': {
      const p = params as CompareGrepParams;
      const loaded = await api.isComparisonLoaded();
      if (!loaded) throw new Error('No comparison project loaded. Use prowl_compare first.');
      const hits = await api.grepComparison(p.pattern, p.file_filter, p.case_sensitive, p.max_results);
      const stats = await api.getComparisonStats();
      return {
        hits,
        count: hits.length,
        note: `Searched ${stats?.cachedFileCount ?? 0} cached files. Use prowl_compare_read_file to fetch more files first.`,
      };
    }

    case 'compare-summary': {
      const loaded = await api.isComparisonLoaded();
      if (!loaded) throw new Error('No comparison project loaded. Use prowl_compare first.');
      return api.getComparisonStats();
    }

    case 'detect-changes': {
      const p = params as DetectChangesParams;
      const scope = p.scope || 'working';
      const projectPath = extra?.projectPath;
      if (!projectPath) {
        throw new Error('No project loaded. Open a project first.');
      }
      if (scope === 'branch' && !p.base_ref) {
        throw new Error('base_ref is required when scope is "branch"');
      }

      // 1. Get changed file paths via git diff IPC
      const changedFiles = await window.prowl.git.diffFiles(projectPath, scope, p.base_ref);
      if (changedFiles.length === 0) {
        return {
          summary: { changed_files: 0, affected_symbols: 0, affected_clusters: 0, risk: 'none' },
          files: [],
          clusters: [],
        };
      }

      // 2. For each changed file, query KuzuDB for symbols
      const fileResults: Array<{ path: string; symbols: string[] }> = [];
      const allSymbolIds: string[] = [];

      for (const filePath of changedFiles) {
        const escaped = filePath.replace(/'/g, "''");
        const rows: any[] = await api.runQuery(`
          MATCH (n) WHERE n.filePath IS NOT NULL AND n.filePath CONTAINS '${escaped}'
            AND label(n) <> 'File' AND label(n) <> 'Folder'
          RETURN n.id AS id, n.name AS name
          LIMIT 100
        `);
        const symbolNames = rows.map((r: any) => r.name || r[1]);
        const symbolIds = rows.map((r: any) => r.id || r[0]);
        fileResults.push({ path: filePath, symbols: symbolNames });
        allSymbolIds.push(...symbolIds);
      }

      // 3. Map symbols → clusters via MEMBER_OF edges
      const clusterMap = new Map<string, { id: string; name: string; count: number }>();

      if (allSymbolIds.length > 0) {
        // Query in batches to avoid overly long Cypher
        const BATCH_SIZE = 50;
        for (let i = 0; i < allSymbolIds.length; i += BATCH_SIZE) {
          const batch = allSymbolIds.slice(i, i + BATCH_SIZE);
          const idList = batch.map(id => `'${String(id).replace(/'/g, "''")}'`).join(', ');
          const clusterRows: any[] = await api.runQuery(`
            MATCH (n)-[r:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
            WHERE n.id IN [${idList}]
            RETURN DISTINCT c.id AS clusterId, c.label AS clusterName, c.heuristicLabel AS hLabel
            LIMIT 200
          `);
          for (const row of clusterRows) {
            const cid = String(row.clusterId || row[0]);
            if (!clusterMap.has(cid)) {
              clusterMap.set(cid, {
                id: cid,
                name: String(row.clusterName || row.hLabel || row[1] || cid),
                count: 0,
              });
            }
          }
        }

        // Count affected symbols per cluster
        if (clusterMap.size > 0) {
          const idList = allSymbolIds.map(id => `'${String(id).replace(/'/g, "''")}'`).join(', ');
          const countRows: any[] = await api.runQuery(`
            MATCH (n)-[r:CodeEdge {type: 'MEMBER_OF'}]->(c:Community)
            WHERE n.id IN [${idList}]
            RETURN c.id AS clusterId, count(n) AS cnt
            LIMIT 200
          `);
          for (const row of countRows) {
            const cid = String(row.clusterId || row[0]);
            const entry = clusterMap.get(cid);
            if (entry) {
              entry.count = Number(row.cnt || row[1] || 0);
            }
          }
        }
      }

      // 4. Compute risk level based on cluster count
      const clusterCount = clusterMap.size;
      let risk: string;
      if (clusterCount === 0) risk = 'low';
      else if (clusterCount <= 2) risk = 'low';
      else if (clusterCount <= 5) risk = 'medium';
      else if (clusterCount <= 10) risk = 'high';
      else risk = 'critical';

      const totalSymbols = fileResults.reduce((sum, f) => sum + f.symbols.length, 0);

      return {
        summary: {
          changed_files: changedFiles.length,
          affected_symbols: totalSymbols,
          affected_clusters: clusterCount,
          risk,
        },
        files: fileResults,
        clusters: Array.from(clusterMap.values()).map(c => ({
          id: c.id,
          name: c.name,
          affected_symbols: c.count,
        })),
      };
    }

    default:
      throw new Error(`Unknown MCP tool: ${toolName}`);
  }
}
