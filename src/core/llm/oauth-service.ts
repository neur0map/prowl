/**
 * OAuth Service
 *
 * Handles OAuth authentication flows for Claude (Anthropic) and OpenAI.
 * Uses Electron deep-link protocol (prowl://oauth/callback) for the redirect.
 * Tokens are stored encrypted via Electron safeStorage.
 *
 * NOTE: OAuth client IDs must be registered with each provider.
 * Set them via environment variables or update the OAUTH_CONFIG below.
 */

export type OAuthProvider = 'anthropic' | 'openai';

interface OAuthConfig {
  clientId: string;
  authUrl: string;
  tokenUrl: string;
  scopes: string[];
  redirectUri: string;
}

interface OAuthTokens {
  accessToken: string;
  refreshToken?: string;
  expiresAt?: number; // Unix timestamp
  email?: string;
}

// OAuth provider configurations
// Client IDs should be set when registered with providers
const OAUTH_CONFIGS: Record<OAuthProvider, OAuthConfig> = {
  anthropic: {
    clientId: import.meta.env.VITE_ANTHROPIC_OAUTH_CLIENT_ID || '',
    authUrl: 'https://console.anthropic.com/oauth/authorize',
    tokenUrl: 'https://api.anthropic.com/oauth/token',
    scopes: ['api:read', 'api:write'],
    redirectUri: 'prowl://oauth/callback',
  },
  openai: {
    clientId: import.meta.env.VITE_OPENAI_OAUTH_CLIENT_ID || '',
    authUrl: 'https://auth.openai.com/authorize',
    tokenUrl: 'https://auth.openai.com/token',
    scopes: ['openid', 'profile', 'email'],
    redirectUri: 'prowl://oauth/callback',
  },
};

// Secure storage keys for OAuth tokens
const TOKEN_KEYS: Record<OAuthProvider, string> = {
  anthropic: 'prowl-oauth-anthropic',
  openai: 'prowl-oauth-openai',
};

// In-memory state
let pendingState: string | null = null;
let pendingProvider: OAuthProvider | null = null;
const tokenCache: Partial<Record<OAuthProvider, OAuthTokens>> = {};

const isElectron = () => typeof window !== 'undefined' && !!(window as any).prowl?.oauth;

/**
 * Generate a random state parameter for CSRF protection
 */
const generateState = (): string => {
  const array = new Uint8Array(32);
  crypto.getRandomValues(array);
  return Array.from(array, b => b.toString(16).padStart(2, '0')).join('');
};

/**
 * Check if OAuth is configured for a provider (has client ID)
 */
export const isOAuthConfigured = (provider: OAuthProvider): boolean => {
  return OAUTH_CONFIGS[provider]?.clientId !== '';
};

/**
 * Check if OAuth is ready to use (Electron + client ID configured)
 */
export const isOAuthReady = (provider: OAuthProvider): boolean => {
  return isElectron() && isOAuthConfigured(provider);
};

/**
 * Check if user is connected via OAuth for a provider
 */
export const isOAuthConnected = (provider: OAuthProvider): boolean => {
  const tokens = tokenCache[provider];
  if (!tokens) return false;
  // Check if expired
  if (tokens.expiresAt && Date.now() > tokens.expiresAt * 1000) return false;
  return true;
};

/**
 * Get the connected user's email for a provider
 */
export const getOAuthEmail = (provider: OAuthProvider): string | null => {
  return tokenCache[provider]?.email ?? null;
};

/**
 * Get the OAuth access token for API calls
 */
export const getOAuthAccessToken = (provider: OAuthProvider): string | null => {
  const tokens = tokenCache[provider];
  if (!tokens) return null;
  if (tokens.expiresAt && Date.now() > tokens.expiresAt * 1000) return null;
  return tokens.accessToken;
};

/**
 * Initialize OAuth — load cached tokens from secure storage
 */
export const initOAuth = async (): Promise<void> => {
  if (!isElectron()) return;

  const prowl = (window as any).prowl;

  for (const [provider, storageKey] of Object.entries(TOKEN_KEYS)) {
    try {
      const stored = await prowl.secureStorage.retrieve(storageKey);
      if (stored) {
        const tokens = JSON.parse(stored) as OAuthTokens;
        tokenCache[provider as OAuthProvider] = tokens;
      }
    } catch {
      // Corrupted token data, ignore
    }
  }

  // Listen for OAuth callbacks from deep links
  prowl.oauth.onCallback(async (data: { code: string | null; state: string | null; error: string | null }) => {
    if (data.error) {
      console.error('OAuth error:', data.error);
      oauthCallbackListeners.forEach(cb => cb({ success: false, error: data.error! }));
      return;
    }

    if (!data.code || !data.state || data.state !== pendingState || !pendingProvider) {
      oauthCallbackListeners.forEach(cb => cb({ success: false, error: 'Invalid OAuth callback' }));
      return;
    }

    try {
      const tokens = await exchangeCodeForTokens(pendingProvider, data.code);
      tokenCache[pendingProvider] = tokens;

      // Store encrypted
      await prowl.secureStorage.store(
        TOKEN_KEYS[pendingProvider],
        JSON.stringify(tokens)
      );

      oauthCallbackListeners.forEach(cb => cb({ success: true, provider: pendingProvider!, email: tokens.email }));
    } catch (error: any) {
      oauthCallbackListeners.forEach(cb => cb({ success: false, error: error.message }));
    } finally {
      pendingState = null;
      pendingProvider = null;
    }
  });
};

/**
 * Start the OAuth flow for a provider
 * Opens the system browser to the provider's consent page
 */
export const startOAuthFlow = async (provider: OAuthProvider): Promise<void> => {
  if (!isElectron()) throw new Error('OAuth requires Electron');

  const config = OAUTH_CONFIGS[provider];
  if (!config.clientId) {
    throw new Error(`OAuth not configured for ${provider}. Set VITE_${provider.toUpperCase()}_OAUTH_CLIENT_ID`);
  }

  pendingState = generateState();
  pendingProvider = provider;

  const params = new URLSearchParams({
    client_id: config.clientId,
    redirect_uri: config.redirectUri,
    response_type: 'code',
    scope: config.scopes.join(' '),
    state: pendingState,
  });

  const authUrl = `${config.authUrl}?${params.toString()}`;
  await (window as any).prowl.oauth.openExternal(authUrl);
};

/**
 * Exchange authorization code for tokens
 */
const exchangeCodeForTokens = async (
  provider: OAuthProvider,
  code: string
): Promise<OAuthTokens> => {
  const config = OAUTH_CONFIGS[provider];

  const response = await fetch(config.tokenUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      grant_type: 'authorization_code',
      client_id: config.clientId,
      code,
      redirect_uri: config.redirectUri,
    }),
  });

  if (!response.ok) {
    const error = await response.text();
    throw new Error(`Token exchange failed: ${error}`);
  }

  const data = await response.json();

  return {
    accessToken: data.access_token,
    refreshToken: data.refresh_token,
    expiresAt: data.expires_in ? Math.floor(Date.now() / 1000) + data.expires_in : undefined,
    email: data.email || data.user?.email,
  };
};

/**
 * Refresh an expired OAuth token
 */
export const refreshOAuthToken = async (provider: OAuthProvider): Promise<boolean> => {
  const tokens = tokenCache[provider];
  if (!tokens?.refreshToken) return false;

  const config = OAUTH_CONFIGS[provider];

  try {
    const response = await fetch(config.tokenUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type: 'refresh_token',
        client_id: config.clientId,
        refresh_token: tokens.refreshToken,
      }),
    });

    if (!response.ok) return false;

    const data = await response.json();
    const newTokens: OAuthTokens = {
      ...tokens,
      accessToken: data.access_token,
      refreshToken: data.refresh_token || tokens.refreshToken,
      expiresAt: data.expires_in ? Math.floor(Date.now() / 1000) + data.expires_in : undefined,
    };

    tokenCache[provider] = newTokens;

    // Update secure storage
    if (isElectron()) {
      const prowl = (window as any).prowl;
      await prowl.secureStorage.store(TOKEN_KEYS[provider], JSON.stringify(newTokens));
    }

    return true;
  } catch {
    return false;
  }
};

/**
 * Sign out — clear tokens for a provider
 */
export const signOutOAuth = async (provider: OAuthProvider): Promise<void> => {
  delete tokenCache[provider];

  if (isElectron()) {
    const prowl = (window as any).prowl;
    await prowl.secureStorage.delete(TOKEN_KEYS[provider]);
  }
};

// ── Callback listeners ──
type OAuthCallbackResult = { success: true; provider: OAuthProvider; email?: string } | { success: false; error: string };
type OAuthCallbackListener = (result: OAuthCallbackResult) => void;
const oauthCallbackListeners: Set<OAuthCallbackListener> = new Set();

/**
 * Register a callback for when OAuth completes
 */
export const onOAuthCallback = (listener: OAuthCallbackListener): (() => void) => {
  oauthCallbackListeners.add(listener);
  return () => oauthCallbackListeners.delete(listener);
};
