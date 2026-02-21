import { useCallback, useEffect, useRef, useState } from 'react';
import { AppStateProvider, useAppState } from './hooks/useAppState';
import { DropZone } from './components/DropZone';
import { LoadingOverlay } from './components/LoadingOverlay';
import { Header } from './components/Header';
import { GraphCanvas, GraphCanvasHandle } from './components/GraphCanvas';
import { RightPanel } from './components/RightPanel';
import { SettingsPanel } from './components/SettingsPanel';
import { RoadmapPanel } from './components/RoadmapPanel';
import { StatusBar } from './components/StatusBar';
import { FileTreePanel } from './components/FileTreePanel';
import { CodeEditorPanel } from './components/CodeEditorPanel';
import { TerminalDrawer } from './components/TerminalDrawer';
import { UpdateBanner } from './components/UpdateBanner';
import { FileEntry } from './services/zip';
import { getActiveProviderConfig } from './core/llm/settings-service';

const AppContent = () => {
  const {
    viewMode,
    setViewMode,
    setGraph,
    setFileContents,
    setProgress,
    setProjectName,
    progress,
    isRightPanelOpen,
    runPipeline,
    runPipelineFromFiles,
    isSettingsPanelOpen,
    setSettingsPanelOpen,
    refreshLLMSettings,
    initializeAgent,
    startEmbeddings,
    embeddingStatus,
    codeReferences,
    selectedNode,
    isCodePanelOpen,
    startAgentWatcher,
    agentWatcherState,
    setProjectPath,
    setLoadedFromSnapshot,
    loadedFromSnapshot,
    saveSnapshot,
    loadSnapshot,
    incrementalUpdate,
    projectPath: currentProjectPath,
  } = useAppState();

  const graphCanvasRef = useRef<GraphCanvasHandle>(null);
  const [isTerminalOpen, setIsTerminalOpen] = useState(false);
  const [isRoadmapOpen, setIsRoadmapOpen] = useState(false);

  // Track folder path for auto-watcher — stored in ref to avoid re-render deps
  const pendingFolderPathRef = useRef<string | null>(null);

  // Keyboard shortcut: Ctrl+` to toggle terminal
  useEffect(() => {
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.ctrlKey && e.key === '`') {
        e.preventDefault();
        setIsTerminalOpen(prev => !prev);
      }
    };
    window.addEventListener('keydown', handleKeyDown);
    return () => window.removeEventListener('keydown', handleKeyDown);
  }, []);

  const handleFileSelect = useCallback(async (file: File) => {
    pendingFolderPathRef.current = null;
    const projectName = file.name.replace('.zip', '');
    setProjectName(projectName);
    setProgress({ phase: 'extracting', percent: 0, message: 'Starting...', detail: 'Preparing to extract files' });
    setViewMode('loading');

    try {
      const result = await runPipeline(file, (progress) => {
        setProgress(progress);
      });

      setGraph(result.graph);
      setFileContents(result.fileContents);
      setViewMode('exploring');

      if (getActiveProviderConfig()) {
        initializeAgent(projectName);
      }

      startEmbeddings().catch((err) => {
        if (err?.name === 'WebGPUNotAvailableError' || err?.message?.includes('WebGPU')) {
          startEmbeddings('wasm').catch(console.warn);
        } else {
          console.warn('Embeddings auto-start failed:', err);
        }
      });
    } catch (error) {
      console.error('Pipeline error:', error);
      setProgress({
        phase: 'error',
        percent: 0,
        message: 'Error processing file',
        detail: error instanceof Error ? error.message : 'Unknown error',
      });
      setTimeout(() => {
        setViewMode('onboarding');
        setProgress(null);
      }, 3000);
    }
  }, [setViewMode, setGraph, setFileContents, setProgress, setProjectName, runPipeline, startEmbeddings, initializeAgent]);

  const handleGitClone = useCallback(async (files: FileEntry[]) => {
    pendingFolderPathRef.current = null;
    const firstPath = files[0]?.path || 'repository';
    const projectName = firstPath.split('/')[0].replace(/-\d+$/, '') || 'repository';

    setProjectName(projectName);
    setProgress({ phase: 'extracting', percent: 0, message: 'Starting...', detail: 'Preparing to process files' });
    setViewMode('loading');

    try {
      const result = await runPipelineFromFiles(files, (progress) => {
        setProgress(progress);
      });

      setGraph(result.graph);
      setFileContents(result.fileContents);
      setViewMode('exploring');

      if (getActiveProviderConfig()) {
        initializeAgent(projectName);
      }

      startEmbeddings().catch((err) => {
        if (err?.name === 'WebGPUNotAvailableError' || err?.message?.includes('WebGPU')) {
          startEmbeddings('wasm').catch(console.warn);
        } else {
          console.warn('Embeddings auto-start failed:', err);
        }
      });
    } catch (error) {
      console.error('Pipeline error:', error);
      setProgress({
        phase: 'error',
        percent: 0,
        message: 'Error processing repository',
        detail: error instanceof Error ? error.message : 'Unknown error',
      });
      setTimeout(() => {
        setViewMode('onboarding');
        setProgress(null);
      }, 3000);
    }
  }, [setViewMode, setGraph, setFileContents, setProgress, setProjectName, runPipelineFromFiles, startEmbeddings, initializeAgent]);

  // Local folder load — checks for snapshot, runs pipeline, then auto-starts the file watcher
  const handleFolderLoad = useCallback(async (files: FileEntry[], folderPath: string) => {
    pendingFolderPathRef.current = folderPath;
    setProjectPath(folderPath);
    const projectName = folderPath.split('/').filter(Boolean).pop() || 'project';

    setProjectName(projectName);
    setLoadedFromSnapshot(false);

    // Check for existing snapshot
    const hasSnapshot = (window as any).prowl?.snapshot
      ? await (window as any).prowl.snapshot.exists(folderPath)
      : false;

    if (hasSnapshot) {
      setProgress({ phase: 'extracting', percent: 0, message: 'Loading cached snapshot...', detail: 'Verifying integrity' });
      setViewMode('loading');

      const result = await loadSnapshot(folderPath, (progress) => {
        setProgress(progress);
      });

      if (result) {
        // Check for incremental changes since snapshot
        let needsIncrementalUpdate = false;
        try {
          const prowlSnapshot = (window as any).prowl?.snapshot;
          if (prowlSnapshot) {
            const meta = await prowlSnapshot.readMeta(folderPath) as any;
            const manifest = await prowlSnapshot.readManifest(folderPath);
            if (meta && manifest) {
              const diff = await prowlSnapshot.detectChanges(folderPath, meta.gitCommit, manifest);
              const totalChanges = (diff.added?.length || 0) + (diff.modified?.length || 0) + (diff.deleted?.length || 0);
              if (totalChanges > 0) {
                needsIncrementalUpdate = true;
                setProgress({ phase: 'structure', percent: 5, message: `Updating ${totalChanges} changed files...` });

                // Run incremental update via hook
                const updatedResult = await incrementalUpdate(diff, folderPath, (p) => setProgress(p));

                if (updatedResult) {
                  setGraph(updatedResult.graph);
                  setFileContents(updatedResult.fileContents);
                } else {
                  // Incremental failed — use snapshot as-is
                  setGraph(result.graph);
                  setFileContents(result.fileContents);
                }

                // Re-save snapshot with updated state
                saveSnapshot(folderPath).catch(console.warn);
              }
            }
          }
        } catch (err) {
          console.warn('[prowl:snapshot] Incremental check failed:', err);
        }

        if (!needsIncrementalUpdate) {
          setGraph(result.graph);
          setFileContents(result.fileContents);
        }

        setViewMode('exploring');
        setLoadedFromSnapshot(true);

        if (getActiveProviderConfig()) {
          initializeAgent(projectName);
        }

        // Embedding status is set by the worker during loadSnapshot
        // but we still need to attempt auto-start if they weren't in the snapshot
        startEmbeddings().catch((err) => {
          if (err?.name === 'WebGPUNotAvailableError' || err?.message?.includes('WebGPU')) {
            startEmbeddings('wasm').catch(console.warn);
          } else {
            console.warn('Embeddings auto-start failed:', err);
          }
        });

        // Auto-start the file watcher
        startAgentWatcher(folderPath).catch((err) => {
          console.warn('Auto-watcher failed to start:', err);
        });

        return; // Skip full pipeline
      }
      // Snapshot load failed — fall through to full pipeline
    }

    setProgress({ phase: 'extracting', percent: 0, message: 'Starting...', detail: 'Preparing to process files' });
    setViewMode('loading');

    try {
      const result = await runPipelineFromFiles(files, (progress) => {
        setProgress(progress);
      });

      setGraph(result.graph);
      setFileContents(result.fileContents);
      setViewMode('exploring');

      if (getActiveProviderConfig()) {
        initializeAgent(projectName);
      }

      startEmbeddings().catch((err) => {
        if (err?.name === 'WebGPUNotAvailableError' || err?.message?.includes('WebGPU')) {
          startEmbeddings('wasm').catch(console.warn);
        } else {
          console.warn('Embeddings auto-start failed:', err);
        }
      });

      // Auto-start the file watcher on the loaded folder
      startAgentWatcher(folderPath).catch((err) => {
        console.warn('Auto-watcher failed to start:', err);
      });

      // Non-blocking background save of snapshot
      saveSnapshot(folderPath).catch(console.warn);
    } catch (error) {
      console.error('Pipeline error:', error);
      setProgress({
        phase: 'error',
        percent: 0,
        message: 'Error processing folder',
        detail: error instanceof Error ? error.message : 'Unknown error',
      });
      setTimeout(() => {
        setViewMode('onboarding');
        setProgress(null);
      }, 3000);
    }
  }, [setViewMode, setGraph, setFileContents, setProgress, setProjectName, runPipelineFromFiles, startEmbeddings, initializeAgent, startAgentWatcher, setProjectPath, setLoadedFromSnapshot, loadSnapshot, saveSnapshot, incrementalUpdate]);

  const handleFocusNode = useCallback((nodeId: string) => {
    graphCanvasRef.current?.focusNode(nodeId);
  }, []);

  const handleRefreshGraph = useCallback(() => {
    graphCanvasRef.current?.refreshGraph();
  }, []);

  const handleSettingsSaved = useCallback(() => {
    refreshLLMSettings();
    initializeAgent();
  }, [refreshLLMSettings, initializeAgent]);

  // Render based on view mode
  if (viewMode === 'onboarding') {
    return (
      <>
        <UpdateBanner />
        <DropZone
          onFileSelect={handleFileSelect}
          onGitClone={handleGitClone}
          onFolderLoad={handleFolderLoad}
        />
      </>
    );
  }

  if (viewMode === 'loading' && progress) {
    return <LoadingOverlay progress={progress} />;
  }

  // Exploring view
  return (
    <div className="flex flex-col h-screen bg-void overflow-hidden">
      <UpdateBanner />
      <Header
        onFocusNode={handleFocusNode}
        onRefreshGraph={handleRefreshGraph}
        onOpenRoadmap={() => setIsRoadmapOpen(true)}
      />

      <main className="flex-1 flex min-h-0 overflow-hidden">
        {/* Left Panel - File Tree */}
        <FileTreePanel onFocusNode={handleFocusNode} />

        {/* Graph area - takes remaining space */}
        <div className="flex-1 relative min-w-0">
          <GraphCanvas ref={graphCanvasRef} />

          {/* Code Editor Panel (overlay) */}
          {isCodePanelOpen && (codeReferences.length > 0 || !!selectedNode) && (
            <div className="absolute inset-y-0 left-0 z-30 pointer-events-auto">
              <CodeEditorPanel onFocusNode={handleFocusNode} />
            </div>
          )}
        </div>

        {/* Right Panel - Code & Chat (tabbed) */}
        {isRightPanelOpen && <RightPanel onFocusNode={handleFocusNode} />}
      </main>

      <StatusBar
        isTerminalOpen={isTerminalOpen}
        onTerminalToggle={() => setIsTerminalOpen(prev => !prev)}
      />

      {/* Terminal Drawer */}
      {(window as any).prowl?.terminal && (
        <TerminalDrawer
          isOpen={isTerminalOpen}
          onToggle={() => setIsTerminalOpen(prev => !prev)}
          cwd={agentWatcherState.workspacePath || pendingFolderPathRef.current || undefined}
        />
      )}

      {/* Settings Panel (modal) */}
      <SettingsPanel
        isOpen={isSettingsPanelOpen}
        onClose={() => setSettingsPanelOpen(false)}
        onSettingsSaved={handleSettingsSaved}
      />

      {/* Roadmap Panel (modal) */}
      <RoadmapPanel
        isOpen={isRoadmapOpen}
        onClose={() => setIsRoadmapOpen(false)}
      />
    </div>
  );
};

function App() {
  return (
    <AppStateProvider>
      <AppContent />
    </AppStateProvider>
  );
}

export default App;
