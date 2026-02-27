/**
 * Animated flow edge for React Flow — bezier curve with a pulsing dot.
 */

import { memo } from 'react';
import { getBezierPath, type EdgeProps } from '@xyflow/react';

function AnimatedEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  data,
  selected,
}: EdgeProps) {
  const weight = (data?.weight as number) || 1;
  const strokeWidth = Math.max(1, Math.log2(weight + 1)) * 1.5;
  const duration = Math.max(1.5, 4 - Math.log2(weight + 1));

  const [edgePath] = getBezierPath({
    sourceX,
    sourceY,
    targetX,
    targetY,
    sourcePosition,
    targetPosition,
  });

  const pathId = `path-${id}`;
  const opacity = selected ? 1 : 0.25;

  return (
    <>
      {/* Invisible wider hit area for hover */}
      <path
        d={edgePath}
        fill="none"
        stroke="transparent"
        strokeWidth={20}
        className="react-flow__edge-interaction"
      />

      {/* Visible edge line */}
      <path
        id={pathId}
        d={edgePath}
        fill="none"
        stroke="rgba(90, 158, 170, 0.6)"
        strokeWidth={strokeWidth}
        opacity={opacity}
        className="transition-opacity duration-300"
      />

      {/* Animated dot */}
      <circle r={3} fill="#5A9EAA" opacity={Math.min(1, opacity + 0.3)}>
        <animateMotion
          dur={`${duration}s`}
          repeatCount="indefinite"
          path={edgePath}
        />
      </circle>

      {/* Weight label on hover — shown via CSS */}
      {selected && (
        <text>
          <textPath
            href={`#${pathId}`}
            startOffset="50%"
            textAnchor="middle"
            className="text-[10px] fill-text-secondary"
            dy={-8}
          >
            {weight} {weight === 1 ? 'link' : 'links'}
          </textPath>
        </text>
      )}
    </>
  );
}

export default memo(AnimatedEdge);
