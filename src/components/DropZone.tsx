import { useState, useCallback, DragEvent } from 'react';
import { Upload, Archive, Github, FolderOpen, Loader2, ArrowRight, Key, Eye, EyeOff } from 'lucide-react';
import { cloneRepository, parseGitHubUrl } from '../services/git-clone';
import { FileEntry } from '../services/zip';

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl;

interface DropZoneProps {
  onFileSelect: (file: File) => void;
  onGitClone?: (files: FileEntry[]) => void;
  onFolderLoad?: (files: FileEntry[], folderPath: string) => void;
}

export const DropZone = ({ onFileSelect, onGitClone, onFolderLoad }: DropZoneProps) => {
  const [isDragging, setIsDragging] = useState(false);
  const [activeTab, setActiveTab] = useState<'zip' | 'github' | 'folder'>('zip');
  const [githubUrl, setGithubUrl] = useState('');
  const [githubToken, setGithubToken] = useState('');
  const [showToken, setShowToken] = useState(false);
  const [isCloning, setIsCloning] = useState(false);
  const [cloneProgress, setCloneProgress] = useState({ phase: '', percent: 0 });
  const [error, setError] = useState<string | null>(null);
  const [isScanning, setIsScanning] = useState(false);
  const [folderPath, setFolderPath] = useState('');
  const [cloneMode, setCloneMode] = useState<'explore' | 'persistent'>('explore');

  const handleDragOver = useCallback((e: DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    setIsDragging(true);
  }, []);

  const handleDragLeave = useCallback((e: DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    setIsDragging(false);
  }, []);

  const handleDrop = useCallback((e: DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    setIsDragging(false);
    const files = e.dataTransfer.files;
    if (files.length > 0) {
      const file = files[0];
      if (file.name.endsWith('.zip')) {
        onFileSelect(file);
      } else {
        setError('Please drop a .zip file');
      }
    }
  }, [onFileSelect]);

  const handleFileInput = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (files && files.length > 0) {
      const file = files[0];
      if (file.name.endsWith('.zip')) {
        onFileSelect(file);
      } else {
        setError('Please select a .zip file');
      }
    }
  }, [onFileSelect]);

  const handleGitClone = async () => {
    if (!githubUrl.trim()) { setError('Please enter a GitHub URL'); return; }
    const parsed = parseGitHubUrl(githubUrl);
    if (!parsed) { setError('Invalid GitHub URL. Use format: https://github.com/owner/repo'); return; }

    setError(null);
    setIsCloning(true);
    setCloneProgress({ phase: 'starting', percent: 0 });

    try {
      const files = await cloneRepository(
        githubUrl,
        (phase, percent) => setCloneProgress({ phase, percent }),
        githubToken || undefined
      );
      setGithubToken('');
      onGitClone?.(files);
    } catch (err) {
      console.error('Clone failed:', err);
      const message = err instanceof Error ? err.message : 'Failed to clone repository';
      if (message.includes('401') || message.includes('403') || message.includes('Authentication')) {
        setError(!githubToken ? 'Private repo — add a GitHub PAT to access it.' : 'Authentication failed. Check token permissions.');
      } else if (message.includes('404') || message.includes('not found')) {
        setError('Repository not found. Check the URL or add a PAT for private repos.');
      } else {
        setError(message);
      }
    } finally {
      setIsCloning(false);
    }
  };

  const scanAndLoad = async (dirPath: string) => {
    setError(null);
    setIsScanning(true);
    try {
      const files = await window.prowl.scanFolder(dirPath);
      if (files.length === 0) { setError('No source files found in this folder.'); return; }
      if (onFolderLoad) {
        onFolderLoad(files, dirPath);
      } else {
        onGitClone?.(files);
      }
    } catch (err) {
      console.error('Folder scan failed:', err);
      setError(err instanceof Error ? err.message : 'Failed to scan folder');
    } finally {
      setIsScanning(false);
    }
  };

  const handleLocalFolder = async () => {
    if (!window.prowl) return;
    setError(null);
    const dirPath = await window.prowl.selectDirectory();
    if (!dirPath) return;
    await scanAndLoad(dirPath);
  };

  const handleFolderPathSubmit = async () => {
    if (!window.prowl || !folderPath.trim()) return;
    await scanAndLoad(folderPath.trim());
  };

  const tabs = [
    { id: 'zip' as const, label: 'ZIP', icon: Archive },
    { id: 'github' as const, label: 'GitHub', icon: Github },
    ...(isElectron ? [{ id: 'folder' as const, label: 'Folder', icon: FolderOpen }] : []),
  ];

  return (
    <div className="flex items-center justify-center min-h-screen p-8 bg-void">
      <div className="relative w-full max-w-md">

        {/* Tab bar */}
        <div className="flex mb-3 border-b border-white/[0.08]">
          {tabs.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              onClick={() => { setActiveTab(id); setError(null); }}
              className={`
                flex items-center gap-1.5 px-3 py-2 text-[13px] transition-all border-b-2 -mb-px
                ${activeTab === id
                  ? 'border-accent text-text-primary'
                  : 'border-transparent text-text-muted hover:text-text-secondary'
                }
              `}
            >
              <Icon className="w-3.5 h-3.5" />
              {label}
            </button>
          ))}
        </div>

        {/* Error */}
        {error && (
          <div className="mb-3 px-3 py-2 rounded-md text-[12px] text-[#FF453A] bg-[#FF453A]/10 border border-[#FF453A]/20">
            {error}
          </div>
        )}

        {/* ZIP tab */}
        {activeTab === 'zip' && (
          <div
            className={`
              p-10 rounded-lg border border-dashed transition-all cursor-pointer
              ${isDragging
                ? 'border-accent bg-accent/5'
                : 'border-white/[0.15] hover:border-white/[0.25] bg-white/[0.03]'
              }
            `}
            onDragOver={handleDragOver}
            onDragLeave={handleDragLeave}
            onDrop={handleDrop}
            onClick={() => document.getElementById('file-input')?.click()}
          >
            <input id="file-input" type="file" accept=".zip" className="hidden" onChange={handleFileInput} />

            <div className={`
              mx-auto w-12 h-12 mb-4 flex items-center justify-center
              rounded-full glass transition-transform
              ${isDragging ? 'scale-110' : ''}
            `}>
              {isDragging
                ? <Upload className="w-5 h-5 text-accent" />
                : <Archive className="w-5 h-5 text-text-secondary" />
              }
            </div>

            <h2 className="text-[15px] font-normal text-text-primary text-center mb-1">
              {isDragging ? 'Drop here' : 'Drop your codebase'}
            </h2>
            <p className="text-[12px] text-text-muted text-center mb-4">
              Drag a .zip file to generate a knowledge graph
            </p>

            <div className="flex justify-center">
              <span className="px-2 py-1 text-[11px] text-text-muted rounded bg-white/[0.06] border border-white/[0.08]">.zip</span>
            </div>
          </div>
        )}

        {/* GitHub tab */}
        {activeTab === 'github' && (
          <div className="p-6 rounded-lg glass">
            <div className="mx-auto w-12 h-12 mb-4 flex items-center justify-center rounded-full bg-white/[0.06] border border-white/[0.1]">
              <Github className="w-5 h-5 text-text-secondary" />
            </div>

            <h2 className="text-[15px] font-normal text-text-primary text-center mb-1">
              Clone from GitHub
            </h2>
            <p className="text-[12px] text-text-muted text-center mb-5">
              Enter a repository URL to clone
            </p>

            <div className="space-y-2.5" data-form-type="other">
              <input
                type="url"
                name="github-repo-url-input"
                value={githubUrl}
                onChange={(e) => setGithubUrl(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && !isCloning && handleGitClone()}
                placeholder="https://github.com/owner/repo"
                disabled={isCloning}
                autoComplete="off"
                data-lpignore="true"
                data-1p-ignore="true"
                data-form-type="other"
                className="w-full px-3 py-2 rounded-md text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/50 disabled:opacity-40 transition-colors"
              />

              <div className="relative">
                <div className="absolute left-2.5 top-1/2 -translate-y-1/2 text-text-muted">
                  <Key className="w-3.5 h-3.5" />
                </div>
                <input
                  type={showToken ? 'text' : 'password'}
                  name="github-pat-token-input"
                  value={githubToken}
                  onChange={(e) => setGithubToken(e.target.value)}
                  placeholder="PAT (optional, private repos)"
                  disabled={isCloning}
                  autoComplete="new-password"
                  data-lpignore="true"
                  data-1p-ignore="true"
                  data-form-type="other"
                  className="w-full pl-8 pr-8 py-2 rounded-md text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/50 disabled:opacity-40 transition-colors"
                />
                <button
                  type="button"
                  onClick={() => setShowToken(!showToken)}
                  className="absolute right-2.5 top-1/2 -translate-y-1/2 text-text-muted hover:text-text-secondary transition-colors"
                >
                  {showToken ? <EyeOff className="w-3.5 h-3.5" /> : <Eye className="w-3.5 h-3.5" />}
                </button>
              </div>

              <button
                onClick={handleGitClone}
                disabled={isCloning || !githubUrl.trim()}
                className="w-full flex items-center justify-center gap-2 px-3 py-2 rounded-md text-[13px] bg-accent text-white hover:bg-accent-dim disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
              >
                {isCloning ? (
                  <>
                    <Loader2 className="w-4 h-4 animate-spin" />
                    {cloneProgress.phase === 'cloning'
                      ? `Cloning ${cloneProgress.percent}%`
                      : cloneProgress.phase === 'reading'
                        ? 'Reading files...'
                        : 'Starting...'
                    }
                  </>
                ) : (
                  <>
                    Clone
                    <ArrowRight className="w-3.5 h-3.5" />
                  </>
                )}
              </button>
            </div>

            {isCloning && (
              <div className="mt-3">
                <div className="h-1 rounded-full overflow-hidden bg-white/[0.08]">
                  <div className="h-full bg-accent transition-all duration-300 ease-out" style={{ width: `${cloneProgress.percent}%` }} />
                </div>
              </div>
            )}

            {githubToken && (
              <p className="mt-2 text-[11px] text-text-muted text-center">Token stays local, never sent to any server</p>
            )}

            <div className="mt-3 flex items-center justify-center gap-2 text-[11px]">
              <button
                onClick={() => setCloneMode('explore')}
                className={`px-2 py-1 rounded transition-colors ${
                  cloneMode === 'explore'
                    ? 'bg-accent/15 text-accent border border-accent/30'
                    : 'bg-white/[0.06] text-text-muted border border-white/[0.08] hover:text-text-secondary'
                }`}
              >
                Explore only
              </button>
              <span className="text-text-muted/30">|</span>
              <button
                onClick={() => setCloneMode('persistent')}
                className={`px-2 py-1 rounded transition-colors ${
                  cloneMode === 'persistent'
                    ? 'bg-accent/15 text-accent border border-accent/30'
                    : 'bg-white/[0.06] text-text-muted border border-white/[0.08] hover:text-text-secondary'
                }`}
              >
                Keep local copy
              </button>
            </div>

            <div className="mt-2 flex justify-center gap-2 text-[11px] text-text-muted">
              <span className="px-2 py-1 rounded bg-white/[0.06] border border-white/[0.08]">
                {githubToken ? 'Private + Public' : 'Public repos'}
              </span>
              <span className="px-2 py-1 rounded bg-white/[0.06] border border-white/[0.08]">Shallow clone</span>
            </div>
          </div>
        )}

        {/* Local Folder tab */}
        {activeTab === 'folder' && (
          <div className="p-6 rounded-lg glass">
            <div className="mx-auto w-12 h-12 mb-4 flex items-center justify-center rounded-full bg-white/[0.06] border border-white/[0.1]">
              <FolderOpen className="w-5 h-5 text-text-secondary" />
            </div>

            <h2 className="text-[15px] font-normal text-text-primary text-center mb-1">
              Open Local Folder
            </h2>
            <p className="text-[12px] text-text-muted text-center mb-5">
              Paste a path or browse for a project folder
            </p>

            <div className="space-y-2.5">
              <div className="flex gap-2">
                <input
                  type="text"
                  value={folderPath}
                  onChange={(e) => setFolderPath(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && !isScanning && handleFolderPathSubmit()}
                  placeholder="/path/to/project or ~/project"
                  disabled={isScanning}
                  autoComplete="off"
                  className="flex-1 px-3 py-2 rounded-md text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/50 disabled:opacity-40 transition-colors font-mono"
                />
                <button
                  onClick={handleLocalFolder}
                  disabled={isScanning}
                  className="px-3 py-2 rounded-md text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-secondary hover:text-text-primary hover:bg-white/[0.1] disabled:opacity-40 transition-colors"
                  title="Browse"
                >
                  <FolderOpen className="w-3.5 h-3.5" />
                </button>
              </div>

              <button
                onClick={handleFolderPathSubmit}
                disabled={isScanning || !folderPath.trim()}
                className="w-full flex items-center justify-center gap-2 px-3 py-2 rounded-md text-[13px] bg-accent text-white hover:bg-accent-dim disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
              >
                {isScanning ? (
                  <><Loader2 className="w-4 h-4 animate-spin" /> Scanning...</>
                ) : (
                  <><ArrowRight className="w-3.5 h-3.5" /> Open</>
                )}
              </button>
            </div>

            <div className="mt-3 flex justify-center gap-2 text-[11px] text-text-muted">
              <span className="px-2 py-1 rounded bg-white/[0.06] border border-white/[0.08]">Any project</span>
              <span className="px-2 py-1 rounded bg-white/[0.06] border border-white/[0.08]">Live watcher</span>
              <span className="px-2 py-1 rounded bg-white/[0.06] border border-white/[0.08]">~/paths</span>
            </div>
          </div>
        )}
      </div>
    </div>
  );
};
