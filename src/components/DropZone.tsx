import { useState } from 'react';
import { FolderOpen, Loader2, Key, Eye, EyeOff, Lock } from 'lucide-react';
import { cloneRepository, parseGitHubUrl } from '../services/git-clone';
import type { FileEntry } from '../types/file-entry';

const isElectron = typeof window !== 'undefined' && !!(window as any).prowl;

interface DropZoneProps {
  onGitClone?: (files: FileEntry[]) => void;
  onFolderLoad?: (files: FileEntry[], folderPath: string) => void;
}

export const DropZone = ({ onGitClone, onFolderLoad }: DropZoneProps) => {
  const [githubUrl, setGithubUrl] = useState('');
  const [githubToken, setGithubToken] = useState('');
  const [showToken, setShowToken] = useState(false);
  const [isCloning, setIsCloning] = useState(false);
  const [cloneProgress, setCloneProgress] = useState({ phase: '', percent: 0 });
  const [error, setError] = useState<string | null>(null);
  const [isScanning, setIsScanning] = useState(false);
  const [folderPath, setFolderPath] = useState('');
  const [showPat, setShowPat] = useState(false);

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

  return (
    <div className="flex items-center justify-center min-h-screen bg-void relative overflow-hidden">
      {/* Ambient glow — matches LoadingOverlay's feel */}
      <div className="absolute inset-0 pointer-events-none">
        <div className="absolute top-1/3 left-1/2 -translate-x-1/2 -translate-y-1/2 w-[500px] h-[500px] bg-accent/[0.03] rounded-full blur-[100px]" />
      </div>

      <div className={`relative w-full px-8 ${isElectron ? 'max-w-[640px]' : 'max-w-sm'}`}>
        {/* Minimal top-left branding (same weight as Header) */}
        <div className="flex items-center gap-2 mb-8">
          <span className="text-white/80 text-sm font-mono">:{'}'}</span>
          <span className="text-[14px] font-normal text-text-primary tracking-tight">Prowl</span>
        </div>

        {/* Error */}
        {error && (
          <div className="mb-4 px-3 py-2 rounded-md text-[12px] text-[#FF453A] bg-[#FF453A]/10 border border-[#FF453A]/20">
            {error}
          </div>
        )}

        {isElectron ? (
          /* ── Electron: folder-first, GitHub secondary ── */
          <div className="space-y-5">
            {/* Folder section — prominent */}
            <div>
              <div className="flex gap-2">
                <input
                  type="text"
                  value={folderPath}
                  onChange={(e) => setFolderPath(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && !isScanning && handleFolderPathSubmit()}
                  placeholder="Paste a path or drop a folder..."
                  disabled={isScanning}
                  autoComplete="off"
                  className="flex-1 px-3 py-2.5 rounded-lg text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/40 disabled:opacity-40 transition-colors font-mono"
                />
                <button
                  onClick={handleLocalFolder}
                  disabled={isScanning}
                  className="px-3 py-2.5 rounded-lg text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-secondary hover:text-text-primary hover:bg-white/[0.1] disabled:opacity-40 transition-colors"
                  title="Browse (⌘O)"
                >
                  <FolderOpen className="w-4 h-4" />
                </button>
              </div>
              <button
                onClick={handleFolderPathSubmit}
                disabled={isScanning || !folderPath.trim()}
                className="mt-2 w-full flex items-center justify-center gap-2 px-3 py-2.5 rounded-lg text-[13px] font-medium bg-accent/90 text-white hover:bg-accent disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
              >
                {isScanning ? (
                  <><Loader2 className="w-4 h-4 animate-spin" /> Scanning...</>
                ) : (
                  'Analyze'
                )}
              </button>
            </div>

            {/* Divider with "or" */}
            <div className="flex items-center gap-3">
              <div className="flex-1 h-px bg-white/[0.06]" />
              <span className="text-[11px] text-text-muted font-mono">or from GitHub</span>
              <div className="flex-1 h-px bg-white/[0.06]" />
            </div>

            {/* GitHub section — compact inline */}
            <div className="space-y-2" data-form-type="other">
              <div className="flex gap-2">
                <input
                  type="url"
                  name="github-repo-url-input"
                  value={githubUrl}
                  onChange={(e) => setGithubUrl(e.target.value)}
                  onKeyDown={(e) => e.key === 'Enter' && !isCloning && handleGitClone()}
                  placeholder="github.com/owner/repo"
                  disabled={isCloning}
                  autoComplete="off"
                  data-lpignore="true"
                  data-1p-ignore="true"
                  data-form-type="other"
                  className="flex-1 px-3 py-2 rounded-lg text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/40 disabled:opacity-40 transition-colors"
                />
                <button
                  onClick={handleGitClone}
                  disabled={isCloning || !githubUrl.trim()}
                  className="px-4 py-2 rounded-lg text-[13px] bg-white/[0.08] border border-white/[0.12] text-text-secondary hover:text-text-primary hover:bg-white/[0.12] disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
                >
                  {isCloning ? (
                    <Loader2 className="w-4 h-4 animate-spin" />
                  ) : (
                    'Fetch'
                  )}
                </button>
              </div>

              {isCloning && (
                <div className="h-[2px] rounded-full overflow-hidden bg-white/[0.06]">
                  <div
                    className="h-full rounded-full transition-all duration-500 ease-out"
                    style={{
                      width: `${cloneProgress.percent}%`,
                      background: 'linear-gradient(90deg, rgba(90,158,170,0.8), rgba(90,158,170,0.4))',
                    }}
                  />
                </div>
              )}

              <p className="text-[11px] text-text-muted">Read-only, nothing saved to disk</p>

              {/* PAT toggle — collapsed by default */}
              <button
                type="button"
                onClick={() => setShowPat(!showPat)}
                className="flex items-center gap-1.5 text-[11px] text-text-muted hover:text-text-secondary transition-colors"
              >
                <Lock className="w-3 h-3" />
                {showPat ? 'Hide token' : 'Private repo?'}
              </button>

              {showPat && (
                <div className="relative">
                  <div className="absolute left-2.5 top-1/2 -translate-y-1/2 text-text-muted">
                    <Key className="w-3.5 h-3.5" />
                  </div>
                  <input
                    type={showToken ? 'text' : 'password'}
                    name="github-pat-token-input"
                    value={githubToken}
                    onChange={(e) => setGithubToken(e.target.value)}
                    placeholder="ghp_..."
                    disabled={isCloning}
                    autoComplete="new-password"
                    data-lpignore="true"
                    data-1p-ignore="true"
                    data-form-type="other"
                    className="w-full pl-8 pr-8 py-2 rounded-lg text-[13px] font-mono bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/40 disabled:opacity-40 transition-colors"
                  />
                  <button
                    type="button"
                    onClick={() => setShowToken(!showToken)}
                    className="absolute right-2.5 top-1/2 -translate-y-1/2 text-text-muted hover:text-text-secondary transition-colors"
                  >
                    {showToken ? <EyeOff className="w-3.5 h-3.5" /> : <Eye className="w-3.5 h-3.5" />}
                  </button>
                </div>
              )}
            </div>
          </div>
        ) : (
          /* ── Browser: GitHub-only, compact ── */
          <div className="space-y-2.5" data-form-type="other">
            <input
              type="url"
              name="github-repo-url-input"
              value={githubUrl}
              onChange={(e) => setGithubUrl(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && !isCloning && handleGitClone()}
              placeholder="github.com/owner/repo"
              disabled={isCloning}
              autoComplete="off"
              data-lpignore="true"
              data-1p-ignore="true"
              data-form-type="other"
              className="w-full px-3 py-2.5 rounded-lg text-[13px] bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/40 disabled:opacity-40 transition-colors"
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
                placeholder="ghp_... (optional, for private repos)"
                disabled={isCloning}
                autoComplete="new-password"
                data-lpignore="true"
                data-1p-ignore="true"
                data-form-type="other"
                className="w-full pl-8 pr-8 py-2.5 rounded-lg text-[13px] font-mono bg-white/[0.06] border border-white/[0.12] text-text-primary placeholder-text-muted focus:outline-none focus:border-accent/40 disabled:opacity-40 transition-colors"
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
              className="w-full flex items-center justify-center gap-2 px-3 py-2.5 rounded-lg text-[13px] font-medium bg-accent/90 text-white hover:bg-accent disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
            >
              {isCloning ? (
                <>
                  <Loader2 className="w-4 h-4 animate-spin" />
                  {cloneProgress.phase === 'cloning'
                    ? `Fetching ${cloneProgress.percent}%`
                    : cloneProgress.phase === 'reading'
                      ? 'Reading files...'
                      : 'Starting...'
                  }
                </>
              ) : (
                'Analyze'
              )}
            </button>

            {isCloning && (
              <div className="h-[2px] rounded-full overflow-hidden bg-white/[0.06]">
                <div
                  className="h-full rounded-full transition-all duration-500 ease-out"
                  style={{
                    width: `${cloneProgress.percent}%`,
                    background: 'linear-gradient(90deg, rgba(90,158,170,0.8), rgba(90,158,170,0.4))',
                  }}
                />
              </div>
            )}

            <p className="text-[11px] text-text-muted">
              Read-only — fetched in-memory{githubToken ? ', token never stored' : ', nothing saved to disk'}
            </p>
          </div>
        )}
      </div>
    </div>
  );
};
