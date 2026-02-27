/**
 * LLM Configuration Persistence
 *
 * Stores provider settings across sessions.
 * Secrets are encrypted via the OS keychain (Electron safeStorage)
 * when running in the desktop app; falls back to plain localStorage
 * in browser-based development mode.
 */

import {
  LLMSettings,
  DEFAULT_LLM_SETTINGS,
  LLMProvider,
  OpenAIConfig,
  AzureOpenAIConfig,
  GeminiConfig,
  AnthropicConfig,
  OllamaConfig,
  OpenRouterConfig,
  GroqConfig,
  ProviderConfig,
} from './types';

const STORAGE_KEY = 'prowl-llm-settings';

/* ── Keychain integration ── */
let secureKeyCache: Record<string, string> = {};
let secureStorageAvailable = false;
let secureStorageInitialized = false;

/* Map each provider to its keychain slot name */
const SECURE_KEY_MAP: Record<string, string> = {
  openai: 'prowl-key-openai',
  'azure-openai': 'prowl-key-azure',
  gemini: 'prowl-key-gemini',
  anthropic: 'prowl-key-anthropic',
  openrouter: 'prowl-key-openrouter',
  groq: 'prowl-key-groq',
};

const isElectron = () => typeof window !== 'undefined' && !!(window as any).prowl?.secureStorage;

/* Bootstrap: pull encrypted keys from the OS keychain and migrate any plain-text remnants */
export const initSecureStorage = async (): Promise<void> => {
  if (secureStorageInitialized) return;
  secureStorageInitialized = true;

  if (!isElectron()) {
    console.log('[prowl:keys] not electron — using localStorage fallback');
    return;
  }

  try {
    const prowl = (window as any).prowl;
    secureStorageAvailable = await prowl.secureStorage.isAvailable();
    console.log('[prowl:keys] secureStorage available:', secureStorageAvailable);
    if (!secureStorageAvailable) return;

    let keysFound = 0;
    for (const [provider, storageKey] of Object.entries(SECURE_KEY_MAP)) {
      const value = await prowl.secureStorage.retrieve(storageKey);
      if (value) {
        secureKeyCache[provider] = value;
        keysFound++;
      }
    }
    console.log(`[prowl:keys] loaded ${keysFound} key(s) from OS keychain`);

    /* Move any residual plain-text keys into the keychain */
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored) {
      const parsed = JSON.parse(stored) as any;
      let migrated = false;

      for (const [provider, storageKey] of Object.entries(SECURE_KEY_MAP)) {
        const providerKey = provider === 'azure-openai' ? 'azureOpenAI' : provider;
        const plaintextKey = parsed[providerKey]?.apiKey;
        if (plaintextKey && plaintextKey.trim() !== '' && !secureKeyCache[provider]) {
          console.log(`[prowl:keys] migrating plain-text key for ${provider} → keychain`);
          await prowl.secureStorage.store(storageKey, plaintextKey);
          secureKeyCache[provider] = plaintextKey;
          if (parsed[providerKey]) {
            parsed[providerKey].apiKey = '';
          }
          migrated = true;
        }
      }

      if (migrated) {
        localStorage.setItem(STORAGE_KEY, JSON.stringify(parsed));
      }
    }
  } catch (error) {
    console.warn('[prowl:keys] failed to initialize secure storage:', error);
  }
};

/* Reports whether the OS keychain was successfully activated */
export const isSecureStorageActive = (): boolean => secureStorageAvailable;

/* Read settings from localStorage, overlaying any keychain-cached secrets */
export const loadSettings = (): LLMSettings => {
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    const parsed = stored ? JSON.parse(stored) as Partial<LLMSettings> : {};

    const settings: LLMSettings = {
      ...DEFAULT_LLM_SETTINGS,
      ...parsed,
      openai: {
        ...DEFAULT_LLM_SETTINGS.openai,
        ...parsed.openai,
      },
      azureOpenAI: {
        ...DEFAULT_LLM_SETTINGS.azureOpenAI,
        ...parsed.azureOpenAI,
      },
      gemini: {
        ...DEFAULT_LLM_SETTINGS.gemini,
        ...parsed.gemini,
      },
      anthropic: {
        ...DEFAULT_LLM_SETTINGS.anthropic,
        ...parsed.anthropic,
      },
      ollama: {
        ...DEFAULT_LLM_SETTINGS.ollama,
        ...parsed.ollama,
      },
      openrouter: {
        ...DEFAULT_LLM_SETTINGS.openrouter,
        ...parsed.openrouter,
      },
      groq: {
        ...DEFAULT_LLM_SETTINGS.groq,
        ...parsed.groq,
      },
    };

    /* Keychain values override any leftover plain-text keys */
    if (secureStorageAvailable) {
      if (secureKeyCache.openai) settings.openai!.apiKey = secureKeyCache.openai;
      if (secureKeyCache['azure-openai']) settings.azureOpenAI!.apiKey = secureKeyCache['azure-openai'];
      if (secureKeyCache.gemini) settings.gemini!.apiKey = secureKeyCache.gemini;
      if (secureKeyCache.anthropic) settings.anthropic!.apiKey = secureKeyCache.anthropic;
      if (secureKeyCache.openrouter) settings.openrouter!.apiKey = secureKeyCache.openrouter;
      if (secureKeyCache.groq) settings.groq!.apiKey = secureKeyCache.groq;
    }

    return settings;
  } catch (error) {
    console.warn('Failed to load LLM settings:', error);
    return DEFAULT_LLM_SETTINGS;
  }
};

/* Write settings to disk; secrets route to the keychain when possible.
 * Returns a promise so callers can await keychain writes before re-initializing the agent. */
export const saveSettings = async (settings: LLMSettings): Promise<void> => {
  try {
    if (secureStorageAvailable && isElectron()) {
      const prowl = (window as any).prowl;
      const keyUpdates: Array<[string, string, string]> = [
        ['openai', SECURE_KEY_MAP.openai, settings.openai?.apiKey ?? ''],
        ['azure-openai', SECURE_KEY_MAP['azure-openai'], settings.azureOpenAI?.apiKey ?? ''],
        ['gemini', SECURE_KEY_MAP.gemini, settings.gemini?.apiKey ?? ''],
        ['anthropic', SECURE_KEY_MAP.anthropic, settings.anthropic?.apiKey ?? ''],
        ['openrouter', SECURE_KEY_MAP.openrouter, settings.openrouter?.apiKey ?? ''],
        ['groq', SECURE_KEY_MAP.groq, settings.groq?.apiKey ?? ''],
      ];

      /* Update in-memory cache immediately so callers see the new keys,
       * then persist to the OS keychain in the background of this await. */
      for (const [provider, storageKey, value] of keyUpdates) {
        if (value.trim()) {
          secureKeyCache[provider] = value;
          try {
            await prowl.secureStorage.store(storageKey, value);
          } catch (err) {
            console.warn(`Failed to store key for ${provider}:`, err);
          }
        } else {
          delete secureKeyCache[provider];
          try {
            await prowl.secureStorage.delete(storageKey);
          } catch {
            /* ignore delete failures */
          }
        }
      }

      /* Strip secrets before writing to localStorage */
      const sanitized = JSON.parse(JSON.stringify(settings));
      if (sanitized.openai) sanitized.openai.apiKey = '';
      if (sanitized.azureOpenAI) sanitized.azureOpenAI.apiKey = '';
      if (sanitized.gemini) sanitized.gemini.apiKey = '';
      if (sanitized.anthropic) sanitized.anthropic.apiKey = '';
      if (sanitized.openrouter) sanitized.openrouter.apiKey = '';
      if (sanitized.groq) sanitized.groq.apiKey = '';
      localStorage.setItem(STORAGE_KEY, JSON.stringify(sanitized));
    } else {
      /* No keychain available — persist everything to localStorage (dev fallback) */
      localStorage.setItem(STORAGE_KEY, JSON.stringify(settings));
    }
  } catch (error) {
    console.error('Failed to save LLM settings:', error);
  }
};

/* Apply partial updates to one provider's config and save */
export const updateProviderSettings = <T extends LLMProvider>(
  provider: T,
  updates: Partial<
    T extends 'openai' ? Partial<Omit<OpenAIConfig, 'provider'>> :
    T extends 'azure-openai' ? Partial<Omit<AzureOpenAIConfig, 'provider'>> :
    T extends 'gemini' ? Partial<Omit<GeminiConfig, 'provider'>> :
    T extends 'anthropic' ? Partial<Omit<AnthropicConfig, 'provider'>> :
    T extends 'ollama' ? Partial<Omit<OllamaConfig, 'provider'>> :
    never
  >
): LLMSettings => {
  const current = loadSettings();

  /* Explicit per-provider branches avoid TS union-spread edge cases */
  switch (provider) {
    case 'openai': {
      const updated: LLMSettings = {
        ...current,
        openai: {
          ...(current.openai ?? {}),
          ...(updates as Partial<Omit<OpenAIConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'azure-openai': {
      const updated: LLMSettings = {
        ...current,
        azureOpenAI: {
          ...(current.azureOpenAI ?? {}),
          ...(updates as Partial<Omit<AzureOpenAIConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'gemini': {
      const updated: LLMSettings = {
        ...current,
        gemini: {
          ...(current.gemini ?? {}),
          ...(updates as Partial<Omit<GeminiConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'anthropic': {
      const updated: LLMSettings = {
        ...current,
        anthropic: {
          ...(current.anthropic ?? {}),
          ...(updates as Partial<Omit<AnthropicConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'ollama': {
      const updated: LLMSettings = {
        ...current,
        ollama: {
          ...(current.ollama ?? {}),
          ...(updates as Partial<Omit<OllamaConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'openrouter': {
      const updated: LLMSettings = {
        ...current,
        openrouter: {
          ...(current.openrouter ?? {}),
          ...(updates as Partial<Omit<OpenRouterConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    case 'groq': {
      const updated: LLMSettings = {
        ...current,
        groq: {
          ...(current.groq ?? {}),
          ...(updates as Partial<Omit<GroqConfig, 'provider'>>),
        },
      };
      saveSettings(updated);
      return updated;
    }
    default: {
      /* Unreachable given T ⊂ LLMProvider, but included as a safety net */
      const updated: LLMSettings = { ...current };
      saveSettings(updated);
      return updated;
    }
  }
};

/* Change which provider is currently selected */
export const setActiveProvider = (provider: LLMProvider): LLMSettings => {
  const current = loadSettings();
  const updated: LLMSettings = {
    ...current,
    activeProvider: provider,
  };
  saveSettings(updated);
  return updated;
};

/* Construct a ProviderConfig for the currently selected provider */
export const getActiveProviderConfig = (): ProviderConfig | null => {
  const settings = loadSettings();

  if (import.meta.env.DEV) {
    const p = settings.activeProvider;
    const hasKey = (k?: string) => !!(k && k.trim());
    const keyInfo: Record<string, boolean> = {
      openai: hasKey(settings.openai?.apiKey),
      gemini: hasKey(settings.gemini?.apiKey),
      anthropic: hasKey(settings.anthropic?.apiKey),
      openrouter: hasKey(settings.openrouter?.apiKey),
      groq: hasKey(settings.groq?.apiKey),
    };
    console.log(`[prowl:keys] active provider: ${p}, keys present:`, keyInfo);
  }

  switch (settings.activeProvider) {
    case 'openai':
      if (!settings.openai?.apiKey) {
        return null;
      }
      return {
        provider: 'openai',
        ...settings.openai,
      } as OpenAIConfig;
      
    case 'azure-openai':
      if (!settings.azureOpenAI?.apiKey || !settings.azureOpenAI?.endpoint) {
        return null;
      }
      return {
        provider: 'azure-openai',
        ...settings.azureOpenAI,
      } as AzureOpenAIConfig;
      
    case 'gemini':
      if (!settings.gemini?.apiKey) {
        return null;
      }
      return {
        provider: 'gemini',
        ...settings.gemini,
      } as GeminiConfig;
      
    case 'anthropic':
      if (!settings.anthropic?.apiKey) {
        return null;
      }
      return {
        provider: 'anthropic',
        ...settings.anthropic,
      } as AnthropicConfig;
      
    case 'ollama':
      return {
        provider: 'ollama',
        ...settings.ollama,
      } as OllamaConfig;
      
    case 'openrouter':
      if (!settings.openrouter?.apiKey || settings.openrouter.apiKey.trim() === '') {
        return null;
      }
      if (!settings.openrouter.model || settings.openrouter.model.trim() === '') {
        return null;
      }
      return {
        provider: 'openrouter',
        apiKey: settings.openrouter.apiKey,
        model: settings.openrouter.model,
        baseUrl: settings.openrouter.baseUrl || 'https://openrouter.ai/api/v1',
        temperature: settings.openrouter.temperature,
        maxTokens: settings.openrouter.maxTokens,
      } as OpenRouterConfig;

    case 'groq':
      if (!settings.groq?.apiKey || settings.groq.apiKey.trim() === '') {
        return null;
      }
      return {
        provider: 'groq',
        apiKey: settings.groq.apiKey,
        model: settings.groq.model || 'llama-3.3-70b-versatile',
        baseUrl: settings.groq.baseUrl || 'https://api.groq.com/openai/v1',
        temperature: settings.groq.temperature,
        maxTokens: settings.groq.maxTokens,
      } as GroqConfig;

    default:
      return null;
  }
};

/* Check that the active provider has every mandatory field populated */
export const isProviderConfigured = (): boolean => {
  return getActiveProviderConfig() !== null;
};

/* Return provider + model for UI display */
export const getActiveProviderDisplay = (): { provider: string; model: string } | null => {
  const config = getActiveProviderConfig();
  if (!config) return null;
  return {
    provider: getProviderDisplayName(config.provider),
    model: config.model || '(no model)',
  };
};

/* Remove all persisted settings; next load returns defaults */
export const clearSettings = (): void => {
  localStorage.removeItem(STORAGE_KEY);
};

/* Wipe all API keys from secure storage AND localStorage, reset cache.
 * Call this when keys appear corrupted or need a clean slate. */
export const clearAllSecureKeys = async (): Promise<void> => {
  /* Clear in-memory cache */
  secureKeyCache = {};

  /* Clear keychain */
  if (isElectron()) {
    try {
      const prowl = (window as any).prowl;
      if (prowl.secureStorage.clearAll) {
        await prowl.secureStorage.clearAll();
      } else {
        /* Fallback: delete one by one */
        for (const storageKey of Object.values(SECURE_KEY_MAP)) {
          await prowl.secureStorage.delete(storageKey).catch(() => {});
        }
      }
    } catch (err) {
      console.warn('[prowl:keys] clearAll failed:', err);
    }
  }

  /* Clear API keys from localStorage too */
  try {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored) {
      const parsed = JSON.parse(stored);
      for (const key of ['openai', 'gemini', 'anthropic', 'openrouter', 'groq']) {
        if (parsed[key]) parsed[key].apiKey = '';
      }
      if (parsed.azureOpenAI) parsed.azureOpenAI.apiKey = '';
      localStorage.setItem(STORAGE_KEY, JSON.stringify(parsed));
    }
  } catch {
    /* ignore */
  }

  console.log('[prowl:keys] all API keys cleared');
};

/* Map a provider key to its display label */
export const getProviderDisplayName = (provider: LLMProvider): string => {
  switch (provider) {
    case 'openai':
      return 'OpenAI';
    case 'azure-openai':
      return 'Azure OpenAI';
    case 'gemini':
      return 'Google Gemini';
    case 'anthropic':
      return 'Anthropic';
    case 'ollama':
      return 'Ollama (Local)';
    case 'openrouter':
      return 'OpenRouter';
    case 'groq':
      return 'Groq';
    default:
      return provider;
  }
};

/* Return a curated list of commonly used models for a given provider */
export const getAvailableModels = (provider: LLMProvider): string[] => {
  switch (provider) {
    case 'openai':
      return ['gpt-4o', 'gpt-4.5-preview', 'gpt-4o-mini', 'gpt-4-turbo', 'gpt-4', 'gpt-3.5-turbo'];
    case 'azure-openai':
      /* Azure names follow the deployment; listing popular choices */
      return ['gpt-4o', 'gpt-4-turbo', 'gpt-4o-mini', 'gpt-4', 'gpt-35-turbo'];
    case 'gemini':
      return ['gemini-2.0-flash', 'gemini-1.5-flash', 'gemini-1.5-pro', 'gemini-1.0-pro'];
    case 'anthropic':
      return ['claude-sonnet-4-20250514', 'claude-3-5-sonnet-20241022', 'claude-3-opus-20240229', 'claude-3-5-haiku-20241022'];
    case 'ollama':
      return ['llama3.2', 'mistral', 'llama3.1', 'codellama', 'deepseek-coder'];
    case 'groq':
      return [
        'llama-3.3-70b-versatile',
        'openai/gpt-oss-120b',
        'llama-3.1-8b-instant',
        'openai/gpt-oss-20b',
        'meta-llama/llama-4-scout-17b-16e-instruct',
        'meta-llama/llama-4-maverick-17b-128e-instruct',
        'qwen/qwen3-32b',
        'moonshotai/kimi-k2-instruct-0905',
      ];
    default:
      return [];
  }
};

/* Fetch the current model catalogue from OpenRouter */
export const fetchOpenRouterModels = async (): Promise<Array<{ id: string; name: string }>> => {
  try {
    const response = await fetch('https://openrouter.ai/api/v1/models');
    if (!response.ok) throw new Error('Failed to fetch models');
    const data = await response.json();
    return data.data.map((model: any) => ({
      id: model.id,
      name: model.name || model.id,
    }));
  } catch (error) {
    console.error('Failed to retrieve OpenRouter model list:', error);
    return [];
  }
};

/* Fetch the current model catalogue from Groq */
export const fetchGroqModels = async (apiKey?: string): Promise<Array<{ id: string; name: string }>> => {
  if (!apiKey || apiKey.trim() === '') return [];
  try {
    const response = await fetch('https://api.groq.com/openai/v1/models', {
      headers: { Authorization: `Bearer ${apiKey}` },
    });
    if (!response.ok) throw new Error('Failed to fetch Groq models');
    const data = await response.json();
    return data.data.map((model: any) => ({
      id: model.id,
      name: model.id,
    }));
  } catch (error) {
    console.error('Failed to retrieve Groq model list:', error);
    return [];
  }
};

