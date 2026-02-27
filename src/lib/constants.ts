import { NodeLabel } from '../core/graph/types';

const NODE_CONFIG: { label: NodeLabel; color: string; size: number }[] = [
  { label: 'Project',     color: '#907040', size: 10 },
  { label: 'Package',     color: '#806838', size: 8 },
  { label: 'Module',      color: '#687060', size: 7 },
  { label: 'Folder',      color: '#A08050', size: 6 },
  { label: 'File',        color: '#7094A8', size: 5 },
  { label: 'Class',       color: '#B85A4A', size: 7 },
  { label: 'Function',    color: '#4A8E6A', size: 4 },
  { label: 'Method',      color: '#3A8E80', size: 4 },
  { label: 'Variable',    color: '#606060', size: 2 },
  { label: 'Interface',   color: '#9A5A80', size: 6 },
  { label: 'Enum',        color: '#B89040', size: 5 },
  { label: 'Decorator',   color: '#907830', size: 2 },
  { label: 'Import',      color: '#505050', size: 2 },
  { label: 'Type',        color: '#7E60A0', size: 4 },
  { label: 'CodeElement', color: '#606060', size: 2 },
  { label: 'Community',   color: '#907040', size: 0 },
  { label: 'Process',     color: '#B05050', size: 0 },
];

export const NODE_COLORS: Record<NodeLabel, string> = NODE_CONFIG.reduce(
  (acc, { label, color }) => { acc[label] = color; return acc; },
  {} as Record<NodeLabel, string>,
);

export const NODE_SIZES: Record<NodeLabel, number> = NODE_CONFIG.reduce(
  (acc, { label, size }) => { acc[label] = size; return acc; },
  {} as Record<NodeLabel, number>,
);

export const MODULE_PALETTE = [
  '#7094A8',
  '#B85A4A',
  '#4A8E6A',
  '#B89040',
  '#3A8E80',
  '#A08050',
  '#7E60A0',
  '#5A7888',
  '#9A5A80',
  '#4A8A78',
  '#B05050',
  '#4A8898',
  '#A87838',
  '#907858',
  '#6A5888',
  '#907040',
];

export const getModuleColor = (communityIndex: number): string => {
  return MODULE_PALETTE[communityIndex % MODULE_PALETTE.length];
};

export const DEFAULT_VISIBLE_LABELS: NodeLabel[] = [
  'Project',
  'Package',
  'Module',
  'Folder',
  'File',
  'Class',
  'Function',
  'Method',
  'Interface',
  'Enum',
  'Type',
];

export const FILTERABLE_LABELS: NodeLabel[] = [
  'Folder',
  'File',
  'Class',
  'Function',
  'Method',
  'Variable',
  'Interface',
  'Import',
];

export type EdgeType = 'CONTAINS' | 'DEFINES' | 'IMPORTS' | 'CALLS' | 'EXTENDS' | 'IMPLEMENTS';

export const ALL_EDGE_TYPES: EdgeType[] = [
  'CONTAINS',
  'DEFINES',
  'IMPORTS',
  'CALLS',
  'EXTENDS',
  'IMPLEMENTS',
];

export const DEFAULT_VISIBLE_EDGES: EdgeType[] = [
  'CONTAINS',
  'DEFINES',
  'IMPORTS',
  'EXTENDS',
  'IMPLEMENTS',
  'CALLS',
];

export const EDGE_INFO: Record<EdgeType, { color: string; label: string }> = {
  CONTAINS:   { color: '#5A7A60', label: 'Contains' },
  DEFINES:    { color: '#5A8090', label: 'Defines' },
  IMPORTS:    { color: '#6A7090', label: 'Imports' },
  CALLS:      { color: '#8A6878', label: 'Calls' },
  EXTENDS:    { color: '#986850', label: 'Extends' },
  IMPLEMENTS: { color: '#885868', label: 'Implements' },
};
