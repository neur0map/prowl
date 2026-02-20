/**
 * Settings Service
 *
 * Handles persistence for LLM provider settings.
 * - Non-sensitive settings (models, endpoints) stored in localStorage
 * - API keys encrypted via Electron safeStorage (OS keychain)
 * - Falls back to localStorage when not running in Electron
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
  ProviderConfig,
} from './types';

const STORAGE_KEY = 'prowl-llm-settings';

// ── Secure Storage ──
// In-memory cache for decrypted API keys (loaded once on init)
let secureKeyCache: Record<string, string> = {};
let secureStorageAvailable = false;
let secureStorageInitialized = false;

// Provider -> secure storage key mapping
const SECURE_KEY_MAP: Record<string, string> = {
  openai: 'prowl-key-openai',
  'azure-openai': 'prowl-key-azure',
  gemini: 'prowl-key-gemini',
  anthropic: 'prowl-key-anthropic',
  openrouter: 'prowl-key-openrouter',
};

const isElectron = () => typeof window !== 'undefined' && !!(window as any).prowl?.secureStorage;

/**
 * Initialize secure storage — call once on app start.
 * Loads all encrypted API keys into memory and migrates plaintext keys if needed.
 */
export const initSecureStorage = async (): Promise<void> => {
  if (secureStorageInitialized) return;
  secureStorageInitialized = true;

  if (!isElectron()) return;

  try {
    const prowl = (window as any).prowl;
    secureStorageAvailable = await prowl.secureStorage.isAvailable();
    if (!secureStorageAvailable) return;

    // Load all encrypted keys into cache
    for (const [provider, storageKey] of Object.entries(SECURE_KEY_MAP)) {
      const value = await prowl.secureStorage.retrieve(storageKey);
      if (value) {
        secureKeyCache[provider] = value;
      }
    }

    // Migrate plaintext keys from localStorage if they exist
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored) {
      const parsed = JSON.parse(stored) as any;
      let migrated = false;

      for (const [provider, storageKey] of Object.entries(SECURE_KEY_MAP)) {
        const providerKey = provider === 'azure-openai' ? 'azureOpenAI' : provider;
        const plaintextKey = parsed[providerKey]?.apiKey;
        if (plaintextKey && plaintextKey.trim() !== '' && !secureKeyCache[provider]) {
          // Migrate to secure storage
          await prowl.secureStorage.store(storageKey, plaintextKey);
          secureKeyCache[provider] = plaintextKey;
          // Strip from localStorage
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
    console.warn('Failed to initialize secure storage:', error);
  }
};

/**
 * Check if secure storage (OS keychain encryption) is active
 */
export const isSecureStorageActive = (): boolean => secureStorageAvailable;

/**
 * Load settings — sync, uses in-memory cache for API keys
 */
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
    };

    // Overlay cached secure keys (overrides any leftover plaintext in localStorage)
    if (secureStorageAvailable) {
      if (secureKeyCache.openai) settings.openai!.apiKey = secureKeyCache.openai;
      if (secureKeyCache['azure-openai']) settings.azureOpenAI!.apiKey = secureKeyCache['azure-openai'];
      if (secureKeyCache.gemini) settings.gemini!.apiKey = secureKeyCache.gemini;
      if (secureKeyCache.anthropic) settings.anthropic!.apiKey = secureKeyCache.anthropic;
      if (secureKeyCache.openrouter) settings.openrouter!.apiKey = secureKeyCache.openrouter;
    }

    return settings;
  } catch (error) {
    console.warn('Failed to load LLM settings:', error);
    return DEFAULT_LLM_SETTINGS;
  }
};

/**
 * Save settings — stores non-sensitive data in localStorage,
 * API keys in encrypted storage when available
 */
export const saveSettings = (settings: LLMSettings): void => {
  try {
    if (secureStorageAvailable && isElectron()) {
      // Save API keys to secure storage (async, fire-and-forget)
      const prowl = (window as any).prowl;
      const keyUpdates: Array<[string, string, string]> = [
        ['openai', SECURE_KEY_MAP.openai, settings.openai?.apiKey ?? ''],
        ['azure-openai', SECURE_KEY_MAP['azure-openai'], settings.azureOpenAI?.apiKey ?? ''],
        ['gemini', SECURE_KEY_MAP.gemini, settings.gemini?.apiKey ?? ''],
        ['anthropic', SECURE_KEY_MAP.anthropic, settings.anthropic?.apiKey ?? ''],
        ['openrouter', SECURE_KEY_MAP.openrouter, settings.openrouter?.apiKey ?? ''],
      ];

      for (const [provider, storageKey, value] of keyUpdates) {
        if (value.trim()) {
          prowl.secureStorage.store(storageKey, value).catch(() => {});
          secureKeyCache[provider] = value;
        } else {
          prowl.secureStorage.delete(storageKey).catch(() => {});
          delete secureKeyCache[provider];
        }
      }

      // Strip API keys from the localStorage copy
      const sanitized = JSON.parse(JSON.stringify(settings));
      if (sanitized.openai) sanitized.openai.apiKey = '';
      if (sanitized.azureOpenAI) sanitized.azureOpenAI.apiKey = '';
      if (sanitized.gemini) sanitized.gemini.apiKey = '';
      if (sanitized.anthropic) sanitized.anthropic.apiKey = '';
      if (sanitized.openrouter) sanitized.openrouter.apiKey = '';
      localStorage.setItem(STORAGE_KEY, JSON.stringify(sanitized));
    } else {
      // Fallback: store everything in localStorage (browser dev mode)
      localStorage.setItem(STORAGE_KEY, JSON.stringify(settings));
    }
  } catch (error) {
    console.error('Failed to save LLM settings:', error);
  }
};

/**
 * Update a specific provider's settings
 */
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

  // Avoid spreading unions like LLMSettings[keyof LLMSettings] (can be string/undefined)
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
    default: {
      // Should be unreachable due to T extends LLMProvider, but keep a safe fallback
      const updated: LLMSettings = { ...current };
      saveSettings(updated);
      return updated;
    }
  }
};

/**
 * Set the active provider
 */
export const setActiveProvider = (provider: LLMProvider): LLMSettings => {
  const current = loadSettings();
  const updated: LLMSettings = {
    ...current,
    activeProvider: provider,
  };
  saveSettings(updated);
  return updated;
};

/**
 * Get the current provider configuration
 */
export const getActiveProviderConfig = (): ProviderConfig | null => {
  const settings = loadSettings();
  
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
      return {
        provider: 'openrouter',
        apiKey: settings.openrouter.apiKey,
        model: settings.openrouter.model || '',
        baseUrl: settings.openrouter.baseUrl || 'https://openrouter.ai/api/v1',
        temperature: settings.openrouter.temperature,
        maxTokens: settings.openrouter.maxTokens,
      } as OpenRouterConfig;
      
    default:
      return null;
  }
};

/**
 * Check if the active provider is properly configured
 */
export const isProviderConfigured = (): boolean => {
  return getActiveProviderConfig() !== null;
};

/**
 * Clear all settings (reset to defaults)
 */
export const clearSettings = (): void => {
  localStorage.removeItem(STORAGE_KEY);
};

/**
 * Get display name for a provider
 */
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
    default:
      return provider;
  }
};

/**
 * Get available models for a provider
 */
export const getAvailableModels = (provider: LLMProvider): string[] => {
  switch (provider) {
    case 'openai':
      return ['gpt-4.5-preview', 'gpt-4o', 'gpt-4o-mini', 'gpt-4-turbo', 'gpt-4', 'gpt-3.5-turbo'];
    case 'azure-openai':
      // Azure models depend on deployment, so we show common ones
      return ['gpt-4o', 'gpt-4o-mini', 'gpt-4-turbo', 'gpt-4', 'gpt-35-turbo'];
    case 'gemini':
      return ['gemini-2.0-flash', 'gemini-1.5-pro', 'gemini-1.5-flash', 'gemini-1.0-pro'];
    case 'anthropic':
      return ['claude-sonnet-4-20250514', 'claude-3-5-sonnet-20241022', 'claude-3-5-haiku-20241022', 'claude-3-opus-20240229'];
    case 'ollama':
      return ['llama3.2', 'llama3.1', 'mistral', 'codellama', 'deepseek-coder'];
    default:
      return [];
  }
};

/**
 * Fetch available models from OpenRouter API
 */
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
    console.error('Error fetching OpenRouter models:', error);
    return [];
  }
};

