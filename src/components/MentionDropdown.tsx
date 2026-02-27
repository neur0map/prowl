import { useEffect, useRef } from 'react';
import { File, Folder } from 'lucide-react';
import type { MentionCandidate } from '../hooks/useAtMention';

interface MentionDropdownProps {
  candidates: MentionCandidate[];
  selectedIndex: number;
  onSelect: (candidate: MentionCandidate) => void;
  onClose: () => void;
}

export const MentionDropdown = ({
  candidates,
  selectedIndex,
  onSelect,
  onClose,
}: MentionDropdownProps) => {
  const listRef = useRef<HTMLDivElement>(null);

  // Scroll selected item into view
  useEffect(() => {
    const list = listRef.current;
    if (!list) return;
    const item = list.children[selectedIndex] as HTMLElement | undefined;
    item?.scrollIntoView({ block: 'nearest' });
  }, [selectedIndex]);

  if (candidates.length === 0) {
    return (
      <div className="absolute bottom-full left-0 right-0 mb-1 bg-deep border border-white/[0.1] rounded-lg shadow-xl overflow-hidden z-50">
        <div className="px-3 py-2 text-[12px] text-text-muted/50 font-mono">
          no matches
        </div>
      </div>
    );
  }

  return (
    <div className="absolute bottom-full left-0 right-0 mb-1 bg-deep border border-white/[0.1] rounded-lg shadow-xl overflow-hidden z-50">
      <div ref={listRef} className="max-h-[200px] overflow-y-auto scrollbar-thin py-1">
        {candidates.map((c, i) => (
          <button
            key={c.id}
            onMouseDown={(e) => {
              e.preventDefault(); // keep textarea focus
              onSelect(c);
            }}
            className={`w-full flex items-center gap-2 px-3 py-1.5 text-left text-[12px] transition-colors ${
              i === selectedIndex
                ? 'bg-accent/10 text-text-primary'
                : 'text-text-secondary hover:bg-white/[0.04]'
            }`}
          >
            {c.label === 'Folder' ? (
              <Folder className="w-3.5 h-3.5 text-text-muted/60 shrink-0" />
            ) : (
              <File className="w-3.5 h-3.5 text-text-muted/60 shrink-0" />
            )}
            <span className="font-mono truncate">{c.name}</span>
            <span className="ml-auto text-[10px] text-text-muted/40 truncate max-w-[40%]">
              {c.filePath}
            </span>
          </button>
        ))}
      </div>
    </div>
  );
};
