import { useState, useEffect, useCallback, useRef, useMemo } from 'react';
import { X, Eye, EyeOff, RefreshCw, ChevronDown, Loader2, Check, AlertCircle } from 'lucide-react';
import {
  loadSettings,
  saveSettings,
  getProviderDisplayName,
  fetchOpenRouterModels,
} from '../core/llm/settings-service';
import type { LLMSettings, LLMProvider } from '../core/llm/types';

// Provider logos — clean SVG marks at their actual brand colors
const logos: Record<LLMProvider, (active: boolean) => JSX.Element> = {
  openai: (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <path d="M22.282 9.821a5.985 5.985 0 0 0-.516-4.91 6.046 6.046 0 0 0-6.51-2.9A6.065 6.065 0 0 0 4.981 4.18a5.998 5.998 0 0 0-3.998 2.9 6.042 6.042 0 0 0 .743 7.097 5.98 5.98 0 0 0 .51 4.911 6.051 6.051 0 0 0 6.515 2.9A5.985 5.985 0 0 0 13.26 24a6.056 6.056 0 0 0 5.772-4.206 5.99 5.99 0 0 0 3.997-2.9 6.056 6.056 0 0 0-.747-7.073zM13.26 22.43a4.476 4.476 0 0 1-2.876-1.04l.141-.081 4.779-2.758a.795.795 0 0 0 .392-.681v-6.737l2.02 1.168a.071.071 0 0 1 .038.052v5.583a4.504 4.504 0 0 1-4.494 4.494zM3.6 18.304a4.47 4.47 0 0 1-.535-3.014l.142.085 4.783 2.759a.771.771 0 0 0 .78 0l5.843-3.369v2.332a.08.08 0 0 1-.033.062L9.74 19.95a4.5 4.5 0 0 1-6.14-1.646zM2.34 7.896a4.485 4.485 0 0 1 2.366-1.973V11.6a.766.766 0 0 0 .388.676l5.815 3.355-2.02 1.168a.076.076 0 0 1-.071 0l-4.83-2.786A4.504 4.504 0 0 1 2.34 7.872zm16.597 3.855l-5.833-3.387L15.119 7.2a.076.076 0 0 1 .071 0l4.83 2.791a4.494 4.494 0 0 1-.676 8.105v-5.678a.79.79 0 0 0-.407-.667zm2.01-3.023l-.141-.085-4.774-2.782a.776.776 0 0 0-.785 0L9.409 9.23V6.897a.066.066 0 0 1 .028-.061l4.83-2.787a4.5 4.5 0 0 1 6.68 4.66zm-12.64 4.135l-2.02-1.164a.08.08 0 0 1-.038-.057V6.075a4.5 4.5 0 0 1 7.375-3.453l-.142.08L8.704 5.46a.795.795 0 0 0-.393.681zm1.097-2.365l2.602-1.5 2.607 1.5v2.999l-2.597 1.5-2.607-1.5z" fill={a ? '#fff' : '#8e8e93'}/>
    </svg>
  ),
  gemini: (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <path d="M12 0C12 6.627 17.373 12 24 12C17.373 12 12 17.373 12 24C12 17.373 6.627 12 0 12C6.627 12 12 6.627 12 0Z" fill={a ? 'url(#gg)' : '#8e8e93'}/>
      <defs><linearGradient id="gg" x1="0" y1="0" x2="24" y2="24"><stop stopColor="#4285F4"/><stop offset="1" stopColor="#D96570"/></linearGradient></defs>
    </svg>
  ),
  anthropic: (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <path d="M13.827 3.52h3.603L24 20.48h-3.603l-6.57-16.96zm-7.257 0h3.604L16.744 20.48h-3.603L6.57 3.52zM0 20.48h3.604L10.174 3.52H6.57L0 20.48z" fill={a ? '#D4A27F' : '#8e8e93'}/>
    </svg>
  ),
  'azure-openai': (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <path d="M5 3l7 18 7-18H5zm7 3.5L15.5 16h-7L12 6.5z" fill={a ? '#0078D4' : '#8e8e93'} fillRule="evenodd"/>
    </svg>
  ),
  ollama: (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <path d="M12 2C8.13 2 5 5.13 5 9c0 2.38 1.19 4.47 3 5.74V17c0 .55.45 1 1 1h1v2c0 .55.45 1 1 1h2c.55 0 1-.45 1-1v-2h1c.55 0 1-.45 1-1v-2.26c1.81-1.27 3-3.36 3-5.74 0-3.87-3.13-7-7-7zm-2 9.5a1.5 1.5 0 1 1 0-3 1.5 1.5 0 0 1 0 3zm4 0a1.5 1.5 0 1 1 0-3 1.5 1.5 0 0 1 0 3z" fill={a ? '#fff' : '#8e8e93'}/>
    </svg>
  ),
  openrouter: (a) => (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none">
      <circle cx="12" cy="12" r="3" fill={a ? '#6366F1' : '#8e8e93'}/>
      <circle cx="4" cy="6" r="1.5" fill={a ? '#6366F1' : '#8e8e93'} opacity="0.5"/>
      <circle cx="20" cy="6" r="1.5" fill={a ? '#6366F1' : '#8e8e93'} opacity="0.5"/>
      <circle cx="4" cy="18" r="1.5" fill={a ? '#6366F1' : '#8e8e93'} opacity="0.5"/>
      <circle cx="20" cy="18" r="1.5" fill={a ? '#6366F1' : '#8e8e93'} opacity="0.5"/>
      <line x1="12" y1="12" x2="4" y2="6" stroke={a ? '#6366F1' : '#8e8e93'} strokeWidth="0.8" opacity="0.3"/>
      <line x1="12" y1="12" x2="20" y2="6" stroke={a ? '#6366F1' : '#8e8e93'} strokeWidth="0.8" opacity="0.3"/>
      <line x1="12" y1="12" x2="4" y2="18" stroke={a ? '#6366F1' : '#8e8e93'} strokeWidth="0.8" opacity="0.3"/>
      <line x1="12" y1="12" x2="20" y2="18" stroke={a ? '#6366F1' : '#8e8e93'} strokeWidth="0.8" opacity="0.3"/>
    </svg>
  ),
};

// Reusable row: label left, input right
const Row = ({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) => (
  <div className="grid grid-cols-[100px_1fr] items-start gap-3 py-2">
    <div className="pt-1.5">
      <span className="text-[12px] text-text-muted">{label}</span>
      {hint && <a href={hint} target="_blank" rel="noopener noreferrer" className="block text-[10px] text-accent/70 hover:text-accent truncate mt-0.5">{hint.replace(/^https?:\/\//, '')}</a>}
    </div>
    <div>{children}</div>
  </div>
);

const fieldClass = 'w-full px-0 py-1.5 bg-transparent border-0 border-b border-white/[0.08] text-[13px] text-text-primary placeholder:text-text-muted/50 focus:border-white/[0.25] focus:outline-none transition-colors';
const monoFieldClass = fieldClass + ' font-mono';

// ── Key input with visibility toggle ──
const KeyInput = ({ value, onChange, visible, onToggle, placeholder }: {
  value: string; onChange: (v: string) => void; visible: boolean; onToggle: () => void; placeholder: string;
}) => (
  <div className="relative">
    <input type={visible ? 'text' : 'password'} value={value} onChange={e => onChange(e.target.value)}
      placeholder={placeholder} className={fieldClass + ' pr-7'} />
    <button type="button" onClick={onToggle} className="absolute right-0 top-1/2 -translate-y-1/2 p-1 text-text-muted/50 hover:text-text-muted">
      {visible ? <EyeOff className="w-3.5 h-3.5" /> : <Eye className="w-3.5 h-3.5" />}
    </button>
  </div>
);

// ── Model presets per provider ──
const MODEL_PRESETS: Record<string, { id: string; name: string }[]> = {
  openai: [
    { id: 'gpt-5.2', name: 'GPT-5.2' },
    { id: 'gpt-5.2-pro', name: 'GPT-5.2 Pro' },
    { id: 'gpt-5.2-chat-latest', name: 'GPT-5.2 Chat' },
    { id: 'gpt-5.1', name: 'GPT-5.1' },
    { id: 'gpt-5', name: 'GPT-5' },
    { id: 'gpt-5-mini', name: 'GPT-5 Mini' },
    { id: 'gpt-5-nano', name: 'GPT-5 Nano' },
    { id: 'o4-mini', name: 'o4 Mini' },
    { id: 'o3', name: 'o3' },
    { id: 'o3-pro', name: 'o3 Pro' },
    { id: 'o3-mini', name: 'o3 Mini' },
    { id: 'gpt-4.1', name: 'GPT-4.1' },
    { id: 'gpt-4.1-mini', name: 'GPT-4.1 Mini' },
    { id: 'gpt-4o', name: 'GPT-4o' },
    { id: 'gpt-4o-mini', name: 'GPT-4o Mini' },
  ],
  gemini: [
    { id: 'gemini-2.5-pro', name: 'Gemini 2.5 Pro' },
    { id: 'gemini-2.5-flash', name: 'Gemini 2.5 Flash' },
    { id: 'gemini-2.0-flash', name: 'Gemini 2.0 Flash' },
    { id: 'gemini-1.5-pro', name: 'Gemini 1.5 Pro' },
    { id: 'gemini-1.5-flash', name: 'Gemini 1.5 Flash' },
  ],
  anthropic: [
    { id: 'claude-opus-4-6', name: 'Claude Opus 4.6' },
    { id: 'claude-sonnet-4-5-20250929', name: 'Claude Sonnet 4.5' },
    { id: 'claude-opus-4-5-20250918', name: 'Claude Opus 4.5' },
    { id: 'claude-sonnet-4-20250514', name: 'Claude Sonnet 4' },
    { id: 'claude-opus-4-20250514', name: 'Claude Opus 4' },
    { id: 'claude-haiku-4-5-20251001', name: 'Claude Haiku 4.5' },
    { id: 'claude-haiku-3-5-20241022', name: 'Claude 3.5 Haiku' },
  ],
  'azure-openai': [
    { id: 'gpt-4o', name: 'GPT-4o' },
    { id: 'gpt-4o-mini', name: 'GPT-4o Mini' },
    { id: 'gpt-4-turbo', name: 'GPT-4 Turbo' },
    { id: 'gpt-4', name: 'GPT-4' },
  ],
  ollama: [
    { id: 'llama3.2', name: 'Llama 3.2' },
    { id: 'llama3.1', name: 'Llama 3.1' },
    { id: 'codellama', name: 'Code Llama' },
    { id: 'deepseek-coder-v2', name: 'DeepSeek Coder v2' },
    { id: 'mistral', name: 'Mistral' },
    { id: 'mixtral', name: 'Mixtral' },
    { id: 'phi-3', name: 'Phi-3' },
    { id: 'qwen2.5-coder', name: 'Qwen 2.5 Coder' },
    { id: 'gemma2', name: 'Gemma 2' },
  ],
};

// ── Unified model selector — presets + search + custom entry ──
interface ModelSelectProps {
  value: string;
  onChange: (v: string) => void;
  models: { id: string; name: string }[];
  isLoading?: boolean;
  onLoad?: () => void;
  placeholder?: string;
}
const ModelSelect = ({ value, onChange, models, isLoading, onLoad, placeholder = 'select model...' }: ModelSelectProps) => {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState('');
  const ref = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const filtered = useMemo(() => {
    if (!q.trim()) return models;
    const l = q.toLowerCase();
    return models.filter(m => m.id.toLowerCase().includes(l) || m.name.toLowerCase().includes(l));
  }, [models, q]);

  const display = useMemo(() => models.find(m => m.id === value)?.name || value, [value, models]);

  useEffect(() => {
    const handler = (e: MouseEvent) => { if (ref.current && !ref.current.contains(e.target as Node)) { setOpen(false); setQ(''); } };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, []);

  return (
    <div ref={ref} className="relative">
      <div onClick={() => { setOpen(true); if (onLoad && !models.length && !isLoading) onLoad(); setTimeout(() => inputRef.current?.focus(), 10); }}
        className={`${fieldClass} cursor-pointer flex items-center`}>
        {open
          ? <input ref={inputRef} value={q} onChange={e => setQ(e.target.value)}
              onKeyDown={e => {
                if (e.key === 'Enter') { const m = filtered[0]; if (m) onChange(m.id); else if (q) onChange(q); setOpen(false); setQ(''); }
                if (e.key === 'Escape') { setOpen(false); setQ(''); }
              }}
              placeholder="search or type custom..." className="flex-1 bg-transparent outline-none text-[13px] font-mono" onClick={e => e.stopPropagation()} />
          : <span className={`flex-1 font-mono truncate ${value ? '' : 'text-text-muted/50'}`}>{display || placeholder}</span>
        }
        {isLoading ? <Loader2 className="w-3 h-3 animate-spin text-text-muted/40" /> : <ChevronDown className={`w-3 h-3 text-text-muted/40 transition-transform ${open ? 'rotate-180' : ''}`} />}
      </div>
      {open && (
        <div className="absolute z-50 w-full mt-1 bg-[#1c1c1e] border border-white/[0.08] rounded-lg shadow-xl overflow-hidden max-h-52 overflow-y-auto scrollbar-thin">
          {isLoading ? <div className="px-3 py-4 text-[11px] text-text-muted text-center">loading...</div>
           : filtered.length === 0 ? <div className="px-3 py-3 text-[11px] text-text-muted text-center">{q ? 'enter to use custom' : 'no models'}</div>
           : filtered.slice(0, 50).map(m => (
              <button key={m.id} onClick={() => { onChange(m.id); setOpen(false); setQ(''); }}
                className={`w-full px-3 py-1.5 text-left text-[12px] hover:bg-white/[0.05] ${m.id === value ? 'text-accent' : 'text-text-secondary'}`}>
                <div className="truncate">{m.name}</div>
                <div className="text-[10px] text-text-muted/50 font-mono truncate">{m.id}</div>
              </button>
            ))}
        </div>
      )}
    </div>
  );
};

// ── Ollama check ──
const checkOllamaStatus = async (baseUrl: string) => {
  try {
    const r = await fetch(`${baseUrl}/api/tags`);
    return r.ok ? { ok: true, error: null } : { ok: false, error: `API error: ${r.status}` };
  } catch { return { ok: false, error: 'Not running. Start with: ollama serve' }; }
};

// ════════════════════════════════════════
// Settings Panel
// ════════════════════════════════════════
interface SettingsPanelProps { isOpen: boolean; onClose: () => void; onSettingsSaved?: () => void; }

export const SettingsPanel = ({ isOpen, onClose, onSettingsSaved }: SettingsPanelProps) => {
  const [settings, setSettings] = useState<LLMSettings>(loadSettings);
  const [showKey, setShowKey] = useState<Record<string, boolean>>({});
  const [saveStatus, setSaveStatus] = useState<'idle' | 'saved' | 'error'>('idle');
  const [ollamaError, setOllamaError] = useState<string | null>(null);
  const [checkingOllama, setCheckingOllama] = useState(false);
  const [orModels, setOrModels] = useState<{ id: string; name: string }[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);

  useEffect(() => { if (isOpen) { setSettings(loadSettings()); setSaveStatus('idle'); setOllamaError(null); } }, [isOpen]);

  const checkOllama = useCallback(async (url: string) => {
    setCheckingOllama(true); setOllamaError(null);
    const { error } = await checkOllamaStatus(url);
    setCheckingOllama(false); setOllamaError(error);
  }, []);

  const loadOrModels = useCallback(async () => {
    setLoadingModels(true); setOrModels(await fetchOpenRouterModels()); setLoadingModels(false);
  }, []);

  useEffect(() => {
    if (settings.activeProvider === 'ollama') {
      const t = setTimeout(() => checkOllama(settings.ollama?.baseUrl ?? 'http://localhost:11434'), 300);
      return () => clearTimeout(t);
    }
  }, [settings.ollama?.baseUrl, settings.activeProvider, checkOllama]);

  const save = () => {
    try { saveSettings(settings); setSaveStatus('saved'); onSettingsSaved?.(); setTimeout(() => setSaveStatus('idle'), 2000); }
    catch { setSaveStatus('error'); }
  };

  if (!isOpen) return null;

  const providers: LLMProvider[] = ['openai', 'gemini', 'anthropic', 'azure-openai', 'ollama', 'openrouter'];
  const p = settings.activeProvider;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/50" onClick={onClose} />

      <div className="relative w-full max-w-[440px] mx-4 bg-[#1c1c1e]/95 backdrop-blur-2xl border border-white/[0.06] rounded-xl shadow-2xl overflow-hidden max-h-[85vh] flex flex-col">

        {/* ── Title bar ── */}
        <div className="flex items-center justify-between px-5 py-3 border-b border-white/[0.06]">
          <span className="text-[13px] font-medium text-text-primary">AI Provider</span>
          <button onClick={onClose} className="text-text-muted hover:text-text-primary transition-colors">
            <X className="w-4 h-4" />
          </button>
        </div>

        {/* ── Provider strip ── */}
        <div className="flex items-center gap-1 px-5 py-3 border-b border-white/[0.06]">
          {providers.map(id => {
            const active = p === id;
            return (
              <button key={id} onClick={() => setSettings(s => ({ ...s, activeProvider: id }))}
                className={`flex items-center gap-1.5 px-2.5 py-1.5 rounded-md text-[11px] transition-all ${
                  active ? 'bg-white/[0.1] text-text-primary' : 'text-text-muted hover:text-text-secondary hover:bg-white/[0.04]'
                }`}
                title={getProviderDisplayName(id)}>
                {logos[id](active)}
                {active && <span className="font-medium">{getProviderDisplayName(id)}</span>}
              </button>
            );
          })}
        </div>

        {/* ── Form ── */}
        <div className="flex-1 overflow-y-auto px-5 py-3 min-h-[240px]">
          <div key={p} className="animate-[fadeSlideIn_150ms_ease-out]">

          {/* OpenAI */}
          {p === 'openai' && <>
            <Row label="API Key" hint="https://platform.openai.com/api-keys">
              <KeyInput value={settings.openai?.apiKey ?? ''} onChange={v => setSettings(s => ({ ...s, openai: { ...s.openai!, apiKey: v } }))}
                visible={!!showKey.openai} onToggle={() => setShowKey(s => ({ ...s, openai: !s.openai }))} placeholder="sk-..." />
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.openai?.model ?? 'gpt-4o'} onChange={m => setSettings(s => ({ ...s, openai: { ...s.openai!, model: m } }))}
                models={MODEL_PRESETS.openai} />
            </Row>
            <Row label="Base URL">
              <input value={settings.openai?.baseUrl ?? ''} onChange={e => setSettings(s => ({ ...s, openai: { ...s.openai!, baseUrl: e.target.value } }))}
                placeholder="default" className={fieldClass} />
            </Row>
          </>}

          {/* Gemini */}
          {p === 'gemini' && <>
            <Row label="API Key" hint="https://aistudio.google.com/app/apikey">
              <KeyInput value={settings.gemini?.apiKey ?? ''} onChange={v => setSettings(s => ({ ...s, gemini: { ...s.gemini!, apiKey: v } }))}
                visible={!!showKey.gemini} onToggle={() => setShowKey(s => ({ ...s, gemini: !s.gemini }))} placeholder="AIza..." />
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.gemini?.model ?? 'gemini-2.0-flash'} onChange={m => setSettings(s => ({ ...s, gemini: { ...s.gemini!, model: m } }))}
                models={MODEL_PRESETS.gemini} />
            </Row>
            <Row label="Endpoint">
              <input value={settings.gemini?.baseUrl ?? ''} onChange={e => setSettings(s => ({ ...s, gemini: { ...s.gemini!, baseUrl: e.target.value } }))}
                placeholder="default" className={fieldClass} />
            </Row>
          </>}

          {/* Anthropic */}
          {p === 'anthropic' && <>
            <Row label="API Key" hint="https://console.anthropic.com/settings/keys">
              <KeyInput value={settings.anthropic?.apiKey ?? ''} onChange={v => setSettings(s => ({ ...s, anthropic: { ...s.anthropic!, apiKey: v } }))}
                visible={!!showKey.anthropic} onToggle={() => setShowKey(s => ({ ...s, anthropic: !s.anthropic }))} placeholder="sk-ant-..." />
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.anthropic?.model ?? 'claude-sonnet-4-20250514'} onChange={m => setSettings(s => ({ ...s, anthropic: { ...s.anthropic!, model: m } }))}
                models={MODEL_PRESETS.anthropic} />
            </Row>
            <Row label="Endpoint">
              <input value={settings.anthropic?.baseUrl ?? ''} onChange={e => setSettings(s => ({ ...s, anthropic: { ...s.anthropic!, baseUrl: e.target.value } }))}
                placeholder="default" className={fieldClass} />
            </Row>
          </>}

          {/* Azure OpenAI */}
          {p === 'azure-openai' && <>
            <Row label="API Key" hint="https://portal.azure.com">
              <KeyInput value={settings.azureOpenAI?.apiKey ?? ''} onChange={v => setSettings(s => ({ ...s, azureOpenAI: { ...s.azureOpenAI!, apiKey: v } }))}
                visible={!!showKey.azure} onToggle={() => setShowKey(s => ({ ...s, azure: !s.azure }))} placeholder="key..." />
            </Row>
            <Row label="Endpoint">
              <input value={settings.azureOpenAI?.endpoint ?? ''} onChange={e => setSettings(s => ({ ...s, azureOpenAI: { ...s.azureOpenAI!, endpoint: e.target.value } }))}
                placeholder="https://your-resource.openai.azure.com" className={fieldClass} />
            </Row>
            <Row label="Deployment">
              <input value={settings.azureOpenAI?.deploymentName ?? ''} onChange={e => setSettings(s => ({ ...s, azureOpenAI: { ...s.azureOpenAI!, deploymentName: e.target.value } }))}
                placeholder="gpt-4o-deployment" className={fieldClass} />
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.azureOpenAI?.model ?? 'gpt-4o'} onChange={m => setSettings(s => ({ ...s, azureOpenAI: { ...s.azureOpenAI!, model: m } }))}
                models={MODEL_PRESETS['azure-openai']} />
            </Row>
            <Row label="API Version">
              <input value={settings.azureOpenAI?.apiVersion ?? '2024-08-01-preview'} onChange={e => setSettings(s => ({ ...s, azureOpenAI: { ...s.azureOpenAI!, apiVersion: e.target.value } }))}
                placeholder="2024-08-01-preview" className={monoFieldClass} />
            </Row>
          </>}

          {/* Ollama */}
          {p === 'ollama' && <>
            <Row label="Base URL">
              <div className="flex items-center gap-2">
                <input value={settings.ollama?.baseUrl ?? 'http://localhost:11434'} onChange={e => setSettings(s => ({ ...s, ollama: { ...s.ollama!, baseUrl: e.target.value } }))}
                  placeholder="http://localhost:11434" className={monoFieldClass + ' flex-1'} />
                <button onClick={() => checkOllama(settings.ollama?.baseUrl ?? 'http://localhost:11434')} disabled={checkingOllama}
                  className="text-text-muted/40 hover:text-text-muted transition-colors">
                  <RefreshCw className={`w-3.5 h-3.5 ${checkingOllama ? 'animate-spin' : ''}`} />
                </button>
              </div>
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.ollama?.model ?? ''} onChange={m => setSettings(s => ({ ...s, ollama: { ...s.ollama!, model: m } }))}
                models={MODEL_PRESETS.ollama} />
            </Row>
            {ollamaError && (
              <div className="flex items-center gap-2 py-2 text-[11px] text-[#FF453A]">
                <AlertCircle className="w-3 h-3 flex-shrink-0" />
                <span>{ollamaError}</span>
              </div>
            )}
          </>}

          {/* OpenRouter */}
          {p === 'openrouter' && <>
            <Row label="API Key" hint="https://openrouter.ai/keys">
              <KeyInput value={settings.openrouter?.apiKey ?? ''} onChange={v => setSettings(s => ({ ...s, openrouter: { ...s.openrouter!, apiKey: v } }))}
                visible={!!showKey.openrouter} onToggle={() => setShowKey(s => ({ ...s, openrouter: !s.openrouter }))} placeholder="sk-or-..." />
            </Row>
            <Row label="Model">
              <ModelSelect value={settings.openrouter?.model ?? ''} onChange={m => setSettings(s => ({ ...s, openrouter: { ...s.openrouter!, model: m } }))}
                models={orModels} isLoading={loadingModels} onLoad={loadOrModels} />
            </Row>
            <Row label="Endpoint">
              <input value={settings.openrouter?.baseUrl ?? ''} onChange={e => setSettings(s => ({ ...s, openrouter: { ...s.openrouter!, baseUrl: e.target.value } }))}
                placeholder="default" className={fieldClass} />
            </Row>
          </>}

          </div>{/* end keyed animation wrapper */}
        </div>

        {/* ── Footer ── */}
        <div className="flex items-center justify-between px-5 py-3 border-t border-white/[0.06]">
          <span className="text-[10px] text-text-muted/40">keys stored locally only</span>
          <div className="flex items-center gap-2">
            {saveStatus === 'saved' && <Check className="w-3.5 h-3.5 text-green-400" />}
            {saveStatus === 'error' && <AlertCircle className="w-3.5 h-3.5 text-[#FF453A]" />}
            <button onClick={onClose} className="px-3 py-1 text-[12px] text-text-muted hover:text-text-primary transition-colors">Cancel</button>
            <button onClick={save} className="px-3 py-1 text-[12px] text-text-primary bg-white/[0.08] hover:bg-white/[0.12] rounded-md transition-colors">Save</button>
          </div>
        </div>
      </div>
    </div>
  );
};
