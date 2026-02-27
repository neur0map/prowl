/**
 * Zone background panel — sits behind cluster cards to visually group zones.
 *
 * Renders a large rounded rectangle with a subtle tinted background
 * and a label in the top-left corner.
 */

import { memo } from 'react';
import type { NodeProps } from '@xyflow/react';
import type { ZoneLabelData } from '../lib/flow-adapter';

const ZONE_BORDER: Record<string, string> = {
  frontend: 'rgba(49, 120, 198, 0.15)',
  backend:  'rgba(222, 165, 132, 0.15)',
  shared:   'rgba(255, 255, 255, 0.05)',
  config:   'rgba(184, 144, 64, 0.12)',
  infra:    'rgba(176, 80, 80, 0.12)',
  docs:     'rgba(120, 170, 120, 0.15)',
};

const ZONE_TEXT: Record<string, string> = {
  frontend: 'rgba(49, 120, 198, 0.5)',
  backend:  'rgba(222, 165, 132, 0.5)',
  shared:   'rgba(255, 255, 255, 0.2)',
  config:   'rgba(184, 144, 64, 0.45)',
  infra:    'rgba(176, 80, 80, 0.45)',
  docs:     'rgba(120, 170, 120, 0.5)',
};

function ZoneLabel({ data }: NodeProps) {
  const { zone, label, color, width, height } = data as ZoneLabelData;

  return (
    <div
      className="rounded-2xl pointer-events-none select-none"
      style={{
        width,
        height,
        background: color,
        border: `1px dashed ${ZONE_BORDER[zone] || 'rgba(255,255,255,0.05)'}`,
      }}
    >
      <div className="px-4 pt-2.5">
        <span
          className="text-[11px] font-semibold uppercase tracking-[0.2em] font-mono"
          style={{ color: ZONE_TEXT[zone] || 'rgba(255,255,255,0.2)' }}
        >
          {label}
        </span>
      </div>
    </div>
  );
}

export default memo(ZoneLabel);
