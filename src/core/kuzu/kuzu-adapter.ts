/**
 * KuzuDB WASM adapter for in-browser graph storage.
 *
 * Maintains an in-memory database and bulk-loads data through
 * CSV files written to the WASM virtual filesystem.
 */

import { CodeGraph } from '../graph/types';
import {
  NODE_TABLES,
  EDGE_TABLE_NAME,
  SCHEMA_QUERIES,
  REL_SCHEMA_QUERIES,
  VECTOR_TABLE,
  NodeTableName,
} from './schema';
import { generateAllCSVs } from './csv-generator';

/* ── Module-level state ──────────────────────────────── */

let wasmLib: any = null;
let database: any = null;
let connection: any = null;

/* Tables that need backtick-escaping in Cypher */
const QUOTED_TABLE_NAMES = new Set<string>([
  'Struct', 'Enum', 'Macro', 'Typedef', 'Union', 'Namespace', 'Trait', 'Impl',
  'TypeAlias', 'Const', 'Static', 'Property', 'Record', 'Delegate', 'Annotation',
  'Constructor', 'Template', 'Module',
]);

/* CSV import flags: RFC 4180 quoting, auto-detect disabled to avoid backslash issues */
const CSV_IMPORT_OPTS = `(HEADER=true, ESCAPE='"', DELIM=',', QUOTE='"', PARALLEL=false, auto_detect=false)`;

/* ── Internal utilities ──────────────────────────────── */

/* Wrap reserved table names in backticks */
const quoteName = (name: string): string =>
  QUOTED_TABLE_NAMES.has(name) ? `\`${name}\`` : name;

/* Build a COPY FROM statement for a given node table */
const makeCopyStatement = (table: NodeTableName, csvPath: string): string => {
  const escaped = quoteName(table);
  switch (table) {
    case 'File':
      return `COPY ${escaped}(id, name, filePath, content) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
    case 'Folder':
      return `COPY ${escaped}(id, name, filePath) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
    case 'Community':
      return `COPY ${escaped}(id, label, heuristicLabel, keywords, description, enrichedBy, cohesion, symbolCount) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
    case 'Process':
      return `COPY ${escaped}(id, label, heuristicLabel, processType, stepCount, communities, entryPointId, terminalId) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
    default: {
      /* Tables with isExported column (matches schema DDL) */
      const TABLES_WITH_IS_EXPORTED = new Set(['Function', 'Class', 'Interface', 'Method', 'CodeElement']);
      if (TABLES_WITH_IS_EXPORTED.has(table)) {
        return `COPY ${escaped}(id, name, filePath, startLine, endLine, isExported, content) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
      }
      return `COPY ${escaped}(id, name, filePath, startLine, endLine, content) FROM "${csvPath}" ${CSV_IMPORT_OPTS}`;
    }
  }
};

/* Derive the table name from a node's ID prefix */
const inferTableFromId = (nodeId: string): string => {
  if (nodeId.startsWith('comm_')) return 'Community';
  if (nodeId.startsWith('proc_')) return 'Process';
  return nodeId.split(':')[0];
};

/* Parse column aliases out of a Cypher RETURN clause */
const extractColumnNames = (cypher: string): string[] => {
  const seg = cypher.match(/RETURN\s+(.+?)(?:\s+ORDER|\s+LIMIT|\s+SKIP|\s*$)/is);
  if (!seg) return [];
  return seg[1].split(',').map((token) => {
    const trimmed = token.trim();
    const aliasHit = trimmed.match(/\s+AS\s+(\w+)\s*$/i);
    if (aliasHit) return aliasHit[1];
    const propHit = trimmed.match(/\.(\w+)\s*$/);
    if (propHit) return propHit[1];
    const fnHit = trimmed.match(/^(\w+)\s*\(/);
    if (fnHit) return fnHit[1];
    return trimmed.replace(/[^a-zA-Z0-9_]/g, '_');
  });
};

/* ── Exported functions ──────────────────────────────── */

/* Start the WASM engine and create an in-memory database */
export const initKuzu = async () => {
  if (connection) return { db: database, conn: connection, kuzu: wasmLib };

  try {
    if (import.meta.env.DEV) console.log('[prowl:kuzu] initializing...');

    if (!wasmLib) {
      const imported = await import('kuzu-wasm');
      wasmLib = imported.default || imported;
      await wasmLib.init();
    }

    const POOL_BYTES = 256 * 1024 * 1024; // 256 MB — fits within WASM 4 GB ceiling
    database = new wasmLib.Database(':memory:', POOL_BYTES);
    connection = new wasmLib.Connection(database);

    if (import.meta.env.DEV) console.log('[prowl:kuzu] wasm initialized');

    /* Execute schema DDL; skip statements that fail (table already exists) */
    let idx = 0;
    while (idx < SCHEMA_QUERIES.length) {
      try {
        await connection.query(SCHEMA_QUERIES[idx]);
      } catch (_schemaErr) {
        const msg = _schemaErr instanceof Error ? _schemaErr.message : String(_schemaErr);
        /* Only warn for unexpected errors, not "already exists" */
        if (import.meta.env.DEV && !msg.includes('already exists')) {
          console.warn(`[prowl:kuzu] DDL[${idx}] failed:`, msg);
        }
      }
      idx++;
    }

    if (import.meta.env.DEV) console.log('[prowl:kuzu] schema created');
    return { db: database, conn: connection, kuzu: wasmLib };
  } catch (err) {
    if (import.meta.env.DEV) console.error('[prowl:kuzu] initialization failed:', err);
    throw err;
  }
};

/* Ingest a full CodeGraph into KuzuDB using CSV bulk import */
export const loadGraphToKuzu = async (
  graph: CodeGraph,
  fileContents: Map<string, string>,
  onProgress?: (percent: number, message: string) => void,
) => {
  /* Always start fresh: close the old DB so the buffer pool is fully
     reclaimed, then create a new instance. Build schema inline rather
     than through initKuzu() — KuzuDB WASM can silently fail DDL when
     reusing the engine after a close/reopen cycle. */
  await closeKuzu();

  if (!wasmLib) {
    const imported = await import('kuzu-wasm');
    wasmLib = imported.default || imported;
    await wasmLib.init();
  }

  const POOL_BYTES = 256 * 1024 * 1024; // 256 MB — fits within WASM 4 GB ceiling
  database = new wasmLib.Database(':memory:', POOL_BYTES);
  connection = new wasmLib.Connection(database);

  /* Run schema DDL and track failures */
  let ddlFails = 0;
  for (let i = 0; i < SCHEMA_QUERIES.length; i++) {
    try {
      await connection.query(SCHEMA_QUERIES[i]);
    } catch (ddlErr) {
      ddlFails++;
      const msg = ddlErr instanceof Error ? ddlErr.message : String(ddlErr);
      if (import.meta.env.DEV && !msg.includes('already exists')) {
        console.warn(`[prowl:kuzu] DDL[${i}] failed:`, msg);
      }
    }
  }

  if (import.meta.env.DEV) {
    console.log(`[prowl:kuzu] fresh DB — schema: ${SCHEMA_QUERIES.length - ddlFails}/${SCHEMA_QUERIES.length} OK`);
  }

  const conn = connection;
  const kuzu = wasmLib;

  /* Wipe any residual data — closeKuzu() can silently fail to destroy
     the DB in WASM, leaving stale rows that cause duplicate-PK errors. */
  for (const tbl of NODE_TABLES) {
    try { await conn.query(`MATCH (n:${quoteName(tbl)}) DELETE n`); } catch { /* table may not exist */ }
  }
  try { await conn.query(`MATCH ()-[e:${EDGE_TABLE_NAME}]->() DELETE e`); } catch { /* noop */ }
  try { await conn.query(`MATCH (e:CodeEmbedding) DELETE e`); } catch { /* noop */ }

  try {
    if (import.meta.env.DEV) console.log(`[prowl:kuzu] generating CSVs for ${graph.nodeCount} nodes`);

    const csvPayload = generateAllCSVs(graph, fileContents);
    const vfs = kuzu.FS;

    /* Write per-table CSV files to the WASM VFS */
    const pendingFiles: Array<{ table: NodeTableName; filePath: string }> = [];
    const tableEntries = Array.from(csvPayload.nodes.entries());
    let tIdx = 0;
    while (tIdx < tableEntries.length) {
      const [tblName, csvText] = tableEntries[tIdx];
      tIdx++;
      /* Skip tables that only have a header row */
      const lineCount = csvText.split('\n').length;
      if (lineCount <= 1) continue;
      const dest = '/' + tblName.toLowerCase() + '.csv';
      try { await vfs.unlink(dest); } catch { /* noop */ }
      await vfs.writeFile(dest, csvText);
      pendingFiles.push({ table: tblName, filePath: dest });
    }

    /* Split the edge CSV into individual rows (header excluded) */
    const relRows = csvPayload.relCSV.split('\n').slice(1).filter((ln: string) => ln.trim());
    const totalRels = relRows.length;

    if (import.meta.env.DEV) {
      console.log(`[prowl:kuzu] wrote ${pendingFiles.length} node CSVs, ${totalRels} relations to insert`);
    }

    /* Import node CSVs first — edges depend on nodes existing */
    let fIdx = 0;
    while (fIdx < pendingFiles.length) {
      const { table, filePath } = pendingFiles[fIdx];
      try {
        await conn.query(makeCopyStatement(table, filePath));
      } catch (copyErr) {
        if (import.meta.env.DEV) {
          console.warn(`[prowl:kuzu] COPY failed for ${table}: ${copyErr instanceof Error ? copyErr.message : String(copyErr)}`);
        }
      }
      fIdx++;
    }

    /* Insert edges one by one (COPY FROM not supported for polymorphic REL tables) */
    const knownTables = new Set<string>(NODE_TABLES as readonly string[]);
    let successCount = 0;
    let failCount = 0;
    const failBuckets = new Map<string, number>();

    const REL_PATTERN = /"([^"]*)","([^"]*)","([^"]*)",([0-9.]+),"([^"]*)",([0-9-]+)/;

    /* Report edge insertion progress every N edges and yield to the event loop */
    const PROGRESS_INTERVAL = 200;

    for (let rIdx = 0; rIdx < relRows.length; rIdx++) {
      const row = relRows[rIdx];
      try {
        const parsed = row.match(REL_PATTERN);
        if (!parsed) continue;

        const srcId = parsed[1];
        const dstId = parsed[2];
        const edgeKind = parsed[3];
        const conf = parseFloat(parsed[4]) || 1.0;
        const rsn = parsed[5];
        const stepVal = parseInt(parsed[6]) || 0;

        const srcTable = inferTableFromId(srcId);
        const dstTable = inferTableFromId(dstId);

        /* Skip edges whose endpoints aren't in the schema */
        if (!knownTables.has(srcTable) || !knownTables.has(dstTable)) {
          failCount++;
          continue;
        }

        const cypher = [
          `MATCH (a:${quoteName(srcTable)} {id: '${srcId.replace(/'/g, "''")}'}),`,
          `      (b:${quoteName(dstTable)} {id: '${dstId.replace(/'/g, "''")}'})`,
          `CREATE (a)-[:${EDGE_TABLE_NAME} {type: '${edgeKind}', confidence: ${conf}, reason: '${rsn.replace(/'/g, "''")}', step: ${stepVal}}]->(b)`,
        ].join('\n');

        await conn.query(cypher);
        successCount++;
      } catch (insertErr) {
        failCount++;
        const m2 = row.match(/"([^"]*)","([^"]*)","([^"]*)",([0-9.]+),"([^"]*)"/);
        if (m2) {
          const bucket = `${m2[3]}:${inferTableFromId(m2[1])}->${inferTableFromId(m2[2])}`;
          failBuckets.set(bucket, (failBuckets.get(bucket) || 0) + 1);
          if (import.meta.env.DEV) {
            console.warn(`[prowl:kuzu] skipped: ${bucket} | "${m2[1]}" -> "${m2[2]}" | ${insertErr instanceof Error ? insertErr.message : String(insertErr)}`);
          }
        }
      }

      /* Periodic progress + yield so the UI thread stays responsive */
      if (rIdx > 0 && rIdx % PROGRESS_INTERVAL === 0) {
        const pct = Math.round((rIdx / totalRels) * 100);
        onProgress?.(pct, `Inserting relationships (${rIdx}/${totalRels})...`);
        await new Promise<void>(r => setTimeout(r, 0));
      }
    }

    if (import.meta.env.DEV) {
      console.log(`[prowl:kuzu] inserted ${successCount}/${totalRels} relations`);
      if (failCount > 0) {
        const ranked = Array.from(failBuckets.entries())
          .sort((a, b) => b[1] - a[1])
          .slice(0, 10);
        console.warn(`[prowl:kuzu] skipped ${failCount}/${totalRels} relations (top by kind/pair):`, ranked);
      }
    }

    /* Tally loaded nodes across all tables for the summary log */
    let nodeTotal = 0;
    for (const tName of NODE_TABLES) {
      try {
        const qr = await conn.query(`MATCH (n:${tName}) RETURN count(n) AS cnt`);
        const r = await qr.getNext();
        nodeTotal += Number(r ? (r.cnt ?? r[0] ?? 0) : 0);
      } catch {
        /* table empty or not created */
      }
    }

    if (import.meta.env.DEV) console.log(`[prowl:kuzu] bulk load complete — ${nodeTotal} nodes, ${successCount} edges`);

    /* Remove temporary CSV files from the VFS */
    pendingFiles.forEach(async ({ filePath }) => {
      try { await vfs.unlink(filePath); } catch { /* noop */ }
    });

    return { success: true, count: nodeTotal };
  } catch (topErr) {
    if (import.meta.env.DEV) console.error('[prowl:kuzu] bulk load failed:', topErr);
    return { success: false, count: 0 };
  }
};

/**
 * Reload graph data into KuzuDB while preserving the CodeEmbedding table.
 * Used by liveUpdate to avoid destroying vector embeddings on every file change.
 *
 * Reuses the existing DB connection — only wipes node/edge tables (NOT CodeEmbedding),
 * then re-imports from fresh CSVs.  Orphaned embedding rows (symbols that no longer
 * exist) are cleaned up at the end.
 */
export const reloadKuzuData = async (
  graph: CodeGraph,
  fileContents: Map<string, string>,
): Promise<{ success: boolean; orphanedEmbeddings: number }> => {
  /* If no DB exists yet, fall through to a full load (no embeddings to preserve) */
  if (!connection || !database) {
    await loadGraphToKuzu(graph, fileContents);
    return { success: true, orphanedEmbeddings: 0 };
  }

  const conn = connection;
  const kuzu = wasmLib;

  try {
    /* 1. Delete all rows from node + edge tables, but NOT CodeEmbedding */
    for (const tbl of NODE_TABLES) {
      try { await conn.query(`MATCH (n:${quoteName(tbl)}) DELETE n`); } catch { /* empty */ }
    }
    try { await conn.query(`MATCH ()-[e:${EDGE_TABLE_NAME}]->() DELETE e`); } catch { /* empty */ }

    /* 2. Generate fresh CSVs */
    const csvPayload = generateAllCSVs(graph, fileContents);
    const vfs = kuzu.FS;

    /* 3. Write and import node CSVs */
    const pendingFiles: Array<{ table: NodeTableName; filePath: string }> = [];
    for (const [tblName, csvText] of csvPayload.nodes.entries()) {
      const lineCount = csvText.split('\n').length;
      if (lineCount <= 1) continue;
      const dest = '/' + tblName.toLowerCase() + '_live.csv';
      try { await vfs.unlink(dest); } catch { /* noop */ }
      await vfs.writeFile(dest, csvText);
      pendingFiles.push({ table: tblName, filePath: dest });
    }

    for (const { table, filePath } of pendingFiles) {
      try {
        await conn.query(makeCopyStatement(table, filePath));
      } catch (copyErr) {
        if (import.meta.env.DEV) {
          console.warn(`[prowl:kuzu:live] COPY failed for ${table}: ${copyErr instanceof Error ? copyErr.message : String(copyErr)}`);
        }
      }
    }

    /* 4. Insert edges one by one */
    const knownTables = new Set<string>(NODE_TABLES as readonly string[]);
    const REL_PATTERN = /"([^"]*)","([^"]*)","([^"]*)",([0-9.]+),"([^"]*)",([0-9-]+)/;
    const relRows = csvPayload.relCSV.split('\n').slice(1).filter((ln: string) => ln.trim());
    let edgeOk = 0;

    for (let rIdx = 0; rIdx < relRows.length; rIdx++) {
      const row = relRows[rIdx];
      try {
        const parsed = row.match(REL_PATTERN);
        if (!parsed) continue;
        const srcId = parsed[1];
        const dstId = parsed[2];
        const edgeKind = parsed[3];
        const conf = parseFloat(parsed[4]) || 1.0;
        const rsn = parsed[5];
        const stepVal = parseInt(parsed[6]) || 0;
        const srcTable = inferTableFromId(srcId);
        const dstTable = inferTableFromId(dstId);
        if (!knownTables.has(srcTable) || !knownTables.has(dstTable)) continue;

        const cypher = [
          `MATCH (a:${quoteName(srcTable)} {id: '${srcId.replace(/'/g, "''")}'}),`,
          `      (b:${quoteName(dstTable)} {id: '${dstId.replace(/'/g, "''")}'})`,
          `CREATE (a)-[:${EDGE_TABLE_NAME} {type: '${edgeKind}', confidence: ${conf}, reason: '${rsn.replace(/'/g, "''")}', step: ${stepVal}}]->(b)`,
        ].join('\n');
        await conn.query(cypher);
        edgeOk++;
      } catch { /* skip bad edge */ }

      /* Yield every 200 edges to keep the worker responsive */
      if (rIdx > 0 && rIdx % 200 === 0) {
        await new Promise<void>(r => setTimeout(r, 0));
      }
    }

    /* 5. Clean up orphaned CodeEmbedding rows — embeddings whose nodeId
          no longer exists in any node table.  We collect valid IDs from
          the in-memory graph (faster than N table scans in KuzuDB). */
    let orphanedEmbeddings = 0;
    try {
      const validNodeIds = new Set<string>();
      for (const node of graph.nodes) validNodeIds.add(node.id);

      // Fetch all current embedding nodeIds
      const embResult = await conn.query(`MATCH (e:${VECTOR_TABLE}) RETURN e.nodeId AS nid`);
      const orphanIds: string[] = [];
      while (await embResult.hasNext()) {
        const row = await embResult.getNext();
        const nid = row?.nid ?? row?.[0];
        if (nid && !validNodeIds.has(nid)) orphanIds.push(nid);
      }

      // Delete orphans in small batches
      for (const oid of orphanIds) {
        try {
          await conn.query(`MATCH (e:${VECTOR_TABLE} {nodeId: '${oid.replace(/'/g, "''")}'}) DELETE e`);
          orphanedEmbeddings++;
        } catch { /* noop */ }
      }
    } catch {
      /* CodeEmbedding table may not exist yet — fine */
    }

    /* 6. Clean up CSV files */
    for (const { filePath } of pendingFiles) {
      try { await vfs.unlink(filePath); } catch { /* noop */ }
    }

    if (import.meta.env.DEV) {
      console.log(`[prowl:kuzu:live] reloaded — ${pendingFiles.length} tables, ${edgeOk} edges, ${orphanedEmbeddings} orphaned embeddings removed`);
    }

    return { success: true, orphanedEmbeddings };
  } catch (err) {
    if (import.meta.env.DEV) console.error('[prowl:kuzu:live] reload failed:', err);
    return { success: false, orphanedEmbeddings: 0 };
  }
};

/* Run a Cypher query and return rows as named-property objects */
export const executeQuery = async (cypher: string): Promise<any[]> => {
  if (!connection || !database) {
    if (import.meta.env.DEV) console.warn('[kuzu] executeQuery: no connection/database');
    return [];
  }

  try {
    const result = await connection.query(cypher);
    const colNames = extractColumnNames(cypher);

    const collected: any[] = [];
    while (await result.hasNext()) {
      const row = await result.getNext();
      if (Array.isArray(row) && colNames.length === row.length) {
        const obj: Record<string, any> = {};
        colNames.forEach((col, i) => { obj[col] = row[i]; });
        collected.push(obj);
      } else {
        collected.push(row);
      }
    }
    return collected;
  } catch (qErr) {
    const msg = qErr instanceof Error ? qErr.message : String(qErr);
    // Gracefully handle closed DB (race with reloadKuzuData or project switch)
    if (msg.includes('database is closed') || msg.includes('not allowed')) {
      if (import.meta.env.DEV) console.warn('[kuzu] DB closed during query, returning empty:', cypher.slice(0, 80));
      return [];
    }
    if (import.meta.env.DEV) console.error('Query execution failed:', qErr);
    throw qErr;
  }
};

/* Aggregate node and edge counts from the database */
export const getKuzuStats = async (): Promise<{ nodes: number; edges: number }> => {
  if (!connection) return { nodes: 0, edges: 0 };

  try {
    let nodeTally = 0;
    for (const tbl of NODE_TABLES) {
      try {
        const res = await connection.query(`MATCH (n:${tbl}) RETURN count(n) AS cnt`);
        const row = await res.getNext();
        nodeTally += Number(row?.cnt ?? row?.[0] ?? 0);
      } catch {
        /* table absent or empty */
      }
    }

    let edgeTally = 0;
    try {
      const eRes = await connection.query(`MATCH ()-[r:${EDGE_TABLE_NAME}]->() RETURN count(r) AS cnt`);
      const eRow = await eRes.getNext();
      edgeTally = Number(eRow?.cnt ?? eRow?.[0] ?? 0);
    } catch {
      /* edge table empty */
    }

    return { nodes: nodeTally, edges: edgeTally };
  } catch (statsErr) {
    if (import.meta.env.DEV) console.warn('Failed to get Kuzu stats:', statsErr);
    return { nodes: 0, edges: 0 };
  }
};

/* Whether the database and connection are live */
export const isKuzuReady = (): boolean => {
  return connection !== null && database !== null;
};

/* Shut down the connection and database handle; keeps WASM loaded for quick restart */
export const closeKuzu = async (): Promise<void> => {
  if (connection) {
    try { await connection.close(); } catch { /* noop */ }
    connection = null;
  }
  if (database) {
    try { await database.close(); } catch { /* noop */ }
    database = null;
  }
};

/* Execute a parameterised Cypher statement via prepare + execute */
export const executePrepared = async (
  cypher: string,
  params: Record<string, any>,
): Promise<any[]> => {
  if (!connection) await initKuzu();

  try {
    const prepared = await connection.prepare(cypher);
    if (!prepared.isSuccess()) {
      const msg = await prepared.getErrorMessage();
      throw new Error(`Prepare failed: ${msg}`);
    }

    const qResult = await connection.execute(prepared, params);
    const rows: any[] = [];
    while (await qResult.hasNext()) {
      rows.push(await qResult.getNext());
    }

    await prepared.close();
    return rows;
  } catch (prepErr) {
    if (import.meta.env.DEV) console.error('Prepared query failed:', prepErr);
    throw prepErr;
  }
};

/* Run a prepared statement against multiple parameter sets, yielding between sub-batches */
export const executeWithReusedStatement = async (
  cypher: string,
  paramsList: Array<Record<string, any>>,
): Promise<void> => {
  if (!connection) await initKuzu();
  if (paramsList.length === 0) return;

  const CHUNK = 32;
  let offset = 0;

  while (offset < paramsList.length) {
    const slice = paramsList.slice(offset, offset + CHUNK);
    const stmt = await connection.prepare(cypher);
    if (!stmt.isSuccess()) {
      const errText = await stmt.getErrorMessage();
      throw new Error(`Prepare failed: ${errText}`);
    }

    try {
      let sIdx = 0;
      while (sIdx < slice.length) {
        await connection.execute(stmt, slice[sIdx]);
        sIdx++;
      }
    } finally {
      await stmt.close();
    }

    offset += CHUNK;
    /* Yield to the event loop between sub-batches */
    if (offset < paramsList.length) {
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    }
  }
};

/* Diagnostic: verify that FLOAT[] array params round-trip correctly */
export const testArrayParams = async (): Promise<{ success: boolean; error?: string }> => {
  if (!connection) await initKuzu();

  try {
    const sampleVec = Array.from({ length: 384 }, (_, pos) => pos / 384);

    /* Locate any existing node to use as a test anchor */
    let anchorId: string | null = null;
    for (const tName of NODE_TABLES) {
      try {
        const probe = await connection.query(`MATCH (n:${tName}) RETURN n.id AS id LIMIT 1`);
        const hit = await probe.getNext();
        if (hit) {
          anchorId = hit.id ?? hit[0];
          break;
        }
      } catch { /* try next table */ }
    }

    if (!anchorId) {
      return { success: false, error: 'No nodes found to test with' };
    }

    if (import.meta.env.DEV) console.log('[prowl:kuzu] testing array params with node:', anchorId);

    /* Insert a synthetic embedding vector */
    const insertCypher = `CREATE (e:${VECTOR_TABLE} {nodeId: $nodeId, embedding: $embedding})`;
    const stmtHandle = await connection.prepare(insertCypher);
    if (!stmtHandle.isSuccess()) {
      const msg = await stmtHandle.getErrorMessage();
      return { success: false, error: `Prepare failed: ${msg}` };
    }

    await connection.execute(stmtHandle, { nodeId: anchorId, embedding: sampleVec });
    await stmtHandle.close();

    /* Read it back and confirm the dimensions match */
    const check = await connection.query(
      `MATCH (e:${VECTOR_TABLE} {nodeId: '${anchorId}'}) RETURN e.embedding AS emb`,
    );
    const checkRow = await check.getNext();
    const stored = checkRow?.emb ?? checkRow?.[0];

    if (stored && Array.isArray(stored) && stored.length === 384) {
      if (import.meta.env.DEV) console.log('[prowl:kuzu] array params work — stored embedding length:', stored.length);
      return { success: true };
    }

    return {
      success: false,
      error: `Embedding not stored correctly. Got: ${typeof stored}, length: ${stored?.length}`,
    };
  } catch (testErr) {
    const detail = testErr instanceof Error ? testErr.message : String(testErr);
    if (import.meta.env.DEV) console.error('[prowl:kuzu] array params test failed:', detail);
    return { success: false, error: detail };
  }
};
