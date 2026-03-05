/**
 * Cluster card — custom React Flow node.
 *
 * Modern glassmorphic card showing module/cluster summary.
 */

import { memo } from 'react';
import { Handle, Position, type NodeProps } from '@xyflow/react';
import { Layers, ArrowUpRight, ArrowDownLeft } from 'lucide-react';
import type { ClusterNodeData } from '../lib/flow-adapter';
import { getModuleColor } from '../lib/constants';
import { ZONE_META } from '../lib/elk-adapter';

const LANG_COLORS: Record<string, string> = {
  TypeScript: '#3178C6',
  JavaScript: '#F0DB4F',
  Python: '#3776AB',
  Rust: '#DEA584',
  Go: '#00ADD8',
  Java: '#B07219',
  Ruby: '#CC342D',
  CSS: '#563D7C',
  HTML: '#E34C26',
  Vue: '#42B883',
  Svelte: '#FF3E00',
  Swift: '#F05138',
  Kotlin: '#A97BFF',
  'C++': '#F34B7D',
  C: '#555555',
  Other: '#666',
};

function ClusterCard({ data, selected }: NodeProps) {
  const { cluster, isHighlighted } = data as ClusterNodeData;
  const accent = getModuleColor(cluster.communityIndex);
  const zoneMeta = ZONE_META[cluster.zone];

  /* Language bar segments */
  const langEntries = Array.from(cluster.languageBreakdown.entries())
    .sort((a, b) => b[1] - a[1]);
  const langTotal = langEntries.reduce((sum, [, c]) => sum + c, 0) || 1;
  const primaryPct = langEntries[0] ? Math.round((langEntries[0][1] / langTotal) * 100) : 0;

  const fileLabel = cluster.fileCount > 0
    ? `${cluster.fileCount} file${cluster.fileCount !== 1 ? 's' : ''}`
    : `${langTotal} source${langTotal !== 1 ? 's' : ''}`;

  return (
    <div
      className={`
        relative rounded-2xl overflow-hidden cursor-pointer
        transition-all duration-300 ease-out group
        ${selected ? 'scale-[1.02]' : isHighlighted ? 'scale-[1.025]' : 'hover:scale-[1.015]'}
      `}
      style={{
        width: 280,
        minHeight: 180,
        background: `linear-gradient(
          135deg,
          rgba(44, 44, 46, 0.75) 0%,
          rgba(38, 38, 40, 0.65) 100%
        )`,
        backdropFilter: 'blur(32px) saturate(180%)',
        WebkitBackdropFilter: 'blur(32px) saturate(180%)',
        border: selected
          ? `1px solid ${accent}55`
          : isHighlighted
            ? `1px solid ${accent}66`
            : '1px solid rgba(255, 255, 255, 0.07)',
        boxShadow: selected
          ? `0 0 0 1px ${accent}22, 0 8px 32px rgba(0, 0, 0, 0.4), 0 0 60px ${accent}11`
          : isHighlighted
            ? `0 0 0 1px ${accent}44, 0 4px 20px ${accent}22, 0 8px 40px rgba(0,0,0,0.4), 0 0 60px ${accent}18`
            : '0 2px 8px rgba(0, 0, 0, 0.2), 0 8px 32px rgba(0, 0, 0, 0.15)',
      }}
    >
      {/* Keyframes for AI tool activity */}
      {isHighlighted && (
        <style>{`
          @keyframes prowl-glow-breathe {
            0%, 100% { opacity: 0.5; }
            50% { opacity: 1; }
          }
          @keyframes prowl-shimmer-sweep {
            0% { transform: translateX(-100%); }
            100% { transform: translateX(200%); }
          }
          @keyframes prowl-border-pulse {
            0%, 100% { opacity: 0.4; }
            50% { opacity: 0.9; }
          }
          @keyframes prowl-dot-ping {
            0% { transform: scale(1); opacity: 1; }
            50% { transform: scale(2.5); opacity: 0; }
            100% { transform: scale(1); opacity: 0; }
          }
        `}</style>
      )}

      {/* Accent gradient at top — animated when highlighted */}
      <div
        className="absolute top-0 left-0 right-0"
        style={{
          height: isHighlighted ? 3 : 2,
          background: isHighlighted
            ? `linear-gradient(90deg, ${accent}88, ${accent}, ${accent}88)`
            : `linear-gradient(90deg, ${accent}, ${accent}44)`,
          boxShadow: isHighlighted ? `0 0 12px ${accent}66, 0 0 4px ${accent}44` : undefined,
          transition: 'all 0.3s ease',
        }}
      />

      {/* Shimmer sweep across top when highlighted */}
      {isHighlighted && (
        <div className="absolute top-0 left-0 right-0 h-[3px] overflow-hidden pointer-events-none">
          <div
            style={{
              width: '40%',
              height: '100%',
              background: `linear-gradient(90deg, transparent, ${accent}cc, white, ${accent}cc, transparent)`,
              animation: 'prowl-shimmer-sweep 2s ease-in-out infinite',
            }}
          />
        </div>
      )}

      {/* Subtle radial glow on hover */}
      <div
        className="absolute inset-0 opacity-0 group-hover:opacity-100 transition-opacity duration-300 pointer-events-none"
        style={{
          background: `radial-gradient(ellipse at 50% 0%, ${accent}08 0%, transparent 70%)`,
        }}
      />

      {/* AI tool activity — layered glow */}
      {isHighlighted && (
        <>
          {/* Inner ambient glow from top */}
          <div
            className="absolute inset-0 pointer-events-none rounded-2xl"
            style={{
              background: `radial-gradient(ellipse at 50% -20%, ${accent}30 0%, transparent 60%)`,
              animation: 'prowl-glow-breathe 2s ease-in-out infinite',
            }}
          />
          {/* Edge glow ring */}
          <div
            className="absolute -inset-[1px] pointer-events-none rounded-2xl"
            style={{
              border: `1px solid ${accent}55`,
              animation: 'prowl-border-pulse 2s ease-in-out infinite',
            }}
          />
          {/* Bottom reflection */}
          <div
            className="absolute inset-x-0 bottom-0 h-1/3 pointer-events-none rounded-b-2xl"
            style={{
              background: `linear-gradient(to top, ${accent}08, transparent)`,
            }}
          />
        </>
      )}

      <Handle type="target" position={Position.Top} className="!bg-transparent !border-0 !w-4 !h-1" />
      <Handle type="source" position={Position.Bottom} className="!bg-transparent !border-0 !w-4 !h-1" />

      <div className="relative p-4 flex flex-col gap-3">
        {/* Header row */}
        <div className="flex items-start justify-between gap-2">
          <div className="flex items-center gap-2.5 min-w-0">
            <div className="relative w-2 h-2 shrink-0">
              <div
                className="w-2 h-2 rounded-full"
                style={{
                  background: accent,
                  boxShadow: isHighlighted ? `0 0 12px ${accent}` : `0 0 8px ${accent}66`,
                }}
              />
              {isHighlighted && (
                <div
                  className="absolute inset-0 rounded-full"
                  style={{
                    background: accent,
                    animation: 'prowl-dot-ping 1.5s ease-out infinite',
                  }}
                />
              )}
            </div>
            <span className="text-[13px] font-semibold text-text-primary truncate leading-tight">
              {cluster.name}
            </span>
          </div>
          {/* Zone badge */}
          {cluster.zone !== 'shared' && (
            <span
              className="text-[9px] font-mono uppercase tracking-wider px-1.5 py-0.5 rounded-md shrink-0"
              style={{
                background: zoneMeta.color,
                color: 'rgba(255, 255, 255, 0.45)',
                border: '1px solid rgba(255, 255, 255, 0.05)',
              }}
            >
              {cluster.zone}
            </span>
          )}
        </div>

        {/* Stats row */}
        <div className="flex items-center gap-3 text-[11px] text-text-secondary font-mono">
          <span className="flex items-center gap-1">
            <Layers size={10} className="text-text-muted" />
            {fileLabel}
          </span>
          <span className="text-text-muted">·</span>
          <span>{cluster.functionCount} fn{cluster.functionCount !== 1 ? 's' : ''}</span>
        </div>

        {/* Language bar */}
        <div className="flex flex-col gap-1.5">
          <div className="flex h-1 rounded-full overflow-hidden" style={{ background: 'rgba(255,255,255,0.04)' }}>
            {langEntries.map(([lang, count]) => (
              <div
                key={lang}
                className="h-full transition-all duration-500"
                style={{
                  width: `${(count / langTotal) * 100}%`,
                  background: LANG_COLORS[lang] || LANG_COLORS.Other,
                  opacity: 0.85,
                }}
                title={`${lang}: ${count}`}
              />
            ))}
          </div>
          <span className="text-[10px] text-text-muted font-mono">
            {primaryPct}% {langEntries[0]?.[0] || 'Unknown'}
          </span>
        </div>

        {/* Exports */}
        {cluster.topExports.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {cluster.topExports.slice(0, 3).map(name => (
              <span
                key={name}
                className="px-1.5 py-0.5 rounded-md text-[10px] font-mono truncate max-w-[120px]"
                style={{
                  background: `${accent}12`,
                  color: `${accent}cc`,
                  border: `1px solid ${accent}18`,
                }}
              >
                {name}
              </span>
            ))}
            {cluster.topExports.length > 3 && (
              <span className="text-[10px] text-text-muted font-mono self-center">
                +{cluster.topExports.length - 3}
              </span>
            )}
          </div>
        )}

        {/* Footer */}
        <div
          className="flex items-center justify-between pt-2 mt-auto"
          style={{ borderTop: '1px solid rgba(255, 255, 255, 0.05)' }}
        >
          <div className="flex items-center gap-3 text-[10px] font-mono text-text-muted">
            {cluster.outDegree > 0 && (
              <span className="flex items-center gap-0.5">
                <ArrowUpRight size={9} />
                {cluster.outDegree}
              </span>
            )}
            {cluster.inDegree > 0 && (
              <span className="flex items-center gap-0.5">
                <ArrowDownLeft size={9} />
                {cluster.inDegree}
              </span>
            )}
          </div>
          <span
            className="text-[9px] font-mono px-1.5 py-0.5 rounded-md"
            style={{
              background: cluster.complexity === 'high' ? 'rgba(239, 68, 68, 0.12)'
                : cluster.complexity === 'moderate' ? 'rgba(234, 179, 8, 0.12)'
                : 'rgba(34, 197, 94, 0.12)',
              color: cluster.complexity === 'high' ? '#ef4444'
                : cluster.complexity === 'moderate' ? '#eab308'
                : '#22c55e',
            }}
          >
            {cluster.complexity === 'high' ? 'complex'
              : cluster.complexity === 'moderate' ? 'moderate' : 'simple'}
          </span>
        </div>
      </div>
    </div>
  );
}

export default memo(ClusterCard);
