/**
 * Breadcrumb navigation bar for the architecture map.
 */

import { memo } from 'react';
import { ChevronRight } from 'lucide-react';

interface Crumb {
  label: string;
  onClick: () => void;
}

interface BreadcrumbNavProps {
  crumbs: Crumb[];
}

function BreadcrumbNav({ crumbs }: BreadcrumbNavProps) {
  return (
    <nav
      className="absolute top-3 left-3 z-20 flex items-center gap-1 px-3 py-1.5 rounded-lg text-xs font-mono"
      style={{
        background: 'rgba(44, 44, 46, 0.75)',
        backdropFilter: 'blur(12px)',
        WebkitBackdropFilter: 'blur(12px)',
        border: '1px solid rgba(255, 255, 255, 0.08)',
      }}
    >
      {crumbs.map((crumb, i) => (
        <span key={i} className="flex items-center gap-1">
          {i > 0 && <ChevronRight size={12} className="text-text-muted" />}
          {i < crumbs.length - 1 ? (
            <button
              onClick={crumb.onClick}
              className="text-text-secondary hover:text-accent transition-colors"
            >
              {crumb.label}
            </button>
          ) : (
            <span className="text-text-primary">{crumb.label}</span>
          )}
        </span>
      ))}
    </nav>
  );
}

export default memo(BreadcrumbNav);
