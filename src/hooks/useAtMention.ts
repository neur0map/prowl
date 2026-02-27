import { useState, useCallback, useRef, useMemo } from 'react';
import { useAppState } from './useAppState';

export interface TaggedFile {
  id: string;          // graph node id
  name: string;        // display name (filename or folder name)
  filePath: string;    // full path
  label: 'File' | 'Folder';
  tokenEstimate: number;
  content: string;     // file content (for prepending to message)
}

export interface MentionCandidate {
  id: string;
  name: string;
  filePath: string;
  label: 'File' | 'Folder';
}

const MAX_FOLDER_FILES = 20;

export function useAtMention() {
  const { graph, fileContents } = useAppState();

  const [taggedFiles, setTaggedFiles] = useState<TaggedFile[]>([]);
  const [mentionQuery, setMentionQuery] = useState('');
  const [isMentionOpen, setIsMentionOpen] = useState(false);
  const [mentionIndex, setMentionIndex] = useState(0);
  const mentionAnchorRef = useRef<number>(-1); // cursor position of the '@'

  // Build candidate list from graph nodes
  const candidates = useMemo<MentionCandidate[]>(() => {
    if (!graph) return [];
    return graph.nodes
      .filter(n => n.label === 'File' || n.label === 'Folder')
      .map(n => ({
        id: n.id,
        name: n.properties.name,
        filePath: n.properties.filePath,
        label: n.label as 'File' | 'Folder',
      }));
  }, [graph]);

  // Filter candidates by query
  const filteredCandidates = useMemo(() => {
    if (!mentionQuery) return candidates.slice(0, 30);
    const q = mentionQuery.toLowerCase();
    return candidates
      .filter(c => c.name.toLowerCase().includes(q) || c.filePath.toLowerCase().includes(q))
      .slice(0, 30);
  }, [candidates, mentionQuery]);

  // Detect '@' in textarea input changes
  const handleInputChange = useCallback((
    value: string,
    cursorPos: number,
  ) => {
    // Look backwards from cursor for an unmatched '@'
    const textBefore = value.slice(0, cursorPos);
    const atIdx = textBefore.lastIndexOf('@');

    if (atIdx >= 0) {
      // Only trigger if '@' is at start or preceded by whitespace
      const charBefore = atIdx > 0 ? value[atIdx - 1] : ' ';
      if (charBefore === ' ' || charBefore === '\n' || atIdx === 0) {
        const query = textBefore.slice(atIdx + 1);
        // Close if there's a space in the query (user moved on)
        if (query.includes(' ') || query.includes('\n')) {
          setIsMentionOpen(false);
          return;
        }
        mentionAnchorRef.current = atIdx;
        setMentionQuery(query);
        setMentionIndex(0);
        setIsMentionOpen(true);
        return;
      }
    }
    setIsMentionOpen(false);
  }, []);

  // Collect child files for a folder
  const collectFolderFiles = useCallback((folderPath: string): { path: string; content: string }[] => {
    const result: { path: string; content: string }[] = [];
    const normFolder = folderPath.replace(/\\/g, '/').replace(/^\.?\//, '');
    const prefix = normFolder.endsWith('/') ? normFolder : normFolder + '/';

    for (const [path, content] of fileContents.entries()) {
      const norm = path.replace(/\\/g, '/').replace(/^\.?\//, '');
      if (norm.startsWith(prefix)) {
        result.push({ path: norm, content });
        if (result.length >= MAX_FOLDER_FILES) break;
      }
    }
    return result;
  }, [fileContents]);

  // Select a candidate
  const selectCandidate = useCallback((candidate: MentionCandidate): {
    newText: string;
    newCursorPos: number;
  } | null => {
    // Check if already tagged
    if (taggedFiles.some(t => t.id === candidate.id)) return null;

    let content = '';
    let tokenEstimate = 0;

    if (candidate.label === 'File') {
      const normPath = candidate.filePath.replace(/\\/g, '/').replace(/^\.?\//, '');
      // Try to find content
      for (const [key, val] of fileContents.entries()) {
        const keyNorm = key.replace(/\\/g, '/').replace(/^\.?\//, '');
        if (keyNorm === normPath) {
          content = val;
          break;
        }
      }
      tokenEstimate = Math.ceil(content.length / 4);
    } else {
      // Folder — collect child files
      const children = collectFolderFiles(candidate.filePath);
      content = children.map(c => `--- ${c.path} ---\n${c.content}`).join('\n\n');
      tokenEstimate = Math.ceil(content.length / 4);
    }

    const tag: TaggedFile = {
      id: candidate.id,
      name: candidate.name,
      filePath: candidate.filePath,
      label: candidate.label,
      tokenEstimate,
      content,
    };

    setTaggedFiles(prev => [...prev, tag]);
    setIsMentionOpen(false);

    return { newText: '', newCursorPos: 0 }; // caller will strip '@query'
  }, [taggedFiles, fileContents, collectFolderFiles]);

  const removeTag = useCallback((id: string) => {
    setTaggedFiles(prev => prev.filter(t => t.id !== id));
  }, []);

  const clearTags = useCallback(() => {
    setTaggedFiles([]);
  }, []);

  // Total token count: input text + tagged content
  const getTokenCount = useCallback((inputText: string): number => {
    const inputTokens = Math.ceil(inputText.length / 4);
    const tagTokens = taggedFiles.reduce((sum, t) => sum + t.tokenEstimate, 0);
    return inputTokens + tagTokens;
  }, [taggedFiles]);

  // Build context block to prepend to message
  const buildContextBlock = useCallback((): string => {
    if (taggedFiles.length === 0) return '';
    return taggedFiles
      .map(t => `<file path="${t.filePath}">\n${t.content}\n</file>`)
      .join('\n\n');
  }, [taggedFiles]);

  return {
    taggedFiles,
    mentionQuery,
    isMentionOpen,
    mentionIndex,
    mentionAnchorRef,
    filteredCandidates,
    setIsMentionOpen,
    setMentionIndex,
    handleInputChange,
    selectCandidate,
    removeTag,
    clearTags,
    getTokenCount,
    buildContextBlock,
  };
}
