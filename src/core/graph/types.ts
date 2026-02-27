/* ── Node taxonomy ─────────────────────────────────────── */

export type NodeLabel =
  | 'Project' | 'Package' | 'Module' | 'Folder' | 'File'
  | 'Class' | 'Function' | 'Method' | 'Variable' | 'Interface'
  | 'Enum' | 'Decorator' | 'Import' | 'Type' | 'CodeElement'
  | 'Community' | 'Process'
  | 'Struct' | 'Const' | 'Static' | 'Property' | 'TypeAlias'
  | 'Macro' | 'Typedef' | 'Union' | 'Namespace' | 'Trait' | 'Impl'
  | 'Record' | 'Delegate' | 'Annotation' | 'Constructor' | 'Template';

/* ── Edge taxonomy ─────────────────────────────────────── */

export type RelationshipType =
  | 'CONTAINS'
  | 'CALLS'
  | 'INHERITS'
  | 'OVERRIDES'
  | 'IMPORTS'
  | 'USES'
  | 'DEFINES'
  | 'DECORATES'
  | 'IMPLEMENTS'
  | 'EXTENDS'
  | 'MEMBER_OF'
  | 'STEP_IN_PROCESS';

/* ── Node shape ────────────────────────────────────────── */

export interface NodeProperties {
  name: string;
  filePath: string;
  startLine?: number;
  endLine?: number;
  language?: string;
  isExported?: boolean;

  /* cluster metadata */
  heuristicLabel?: string;
  cohesion?: number;
  symbolCount?: number;
  keywords?: string[];
  description?: string;
  enrichedBy?: 'heuristic' | 'llm';

  /* detected execution flow */
  processType?: 'intra_community' | 'cross_community';
  stepCount?: number;
  communities?: string[];
  entryPointId?: string;
  terminalId?: string;

  /* importance ranking */
  entryPointScore?: number;
  entryPointReason?: string;
}

export interface GraphNode {
  id: string;
  label: NodeLabel;
  properties: NodeProperties;
}

/* ── Edge shape ────────────────────────────────────────── */

export interface GraphRelationship {
  id: string;
  sourceId: string;
  targetId: string;
  type: RelationshipType;
  /** 0–1 certainty (1 = definite, lower = heuristic match) */
  confidence: number;
  /** How the link was inferred: 'import-resolved' | 'same-file' | 'fuzzy-global' | '' */
  reason: string;
  /** Ordinal within a STEP_IN_PROCESS chain (starts at 1) */
  step?: number;
}

/* ── Mutable graph container ───────────────────────────── */

export interface CodeGraph {
  nodes: GraphNode[];
  relationships: GraphRelationship[];
  nodeCount: number;
  relationshipCount: number;
  addNode(node: GraphNode): void;
  addRelationship(rel: GraphRelationship): void;
  hasNode(id: string): boolean;
}
