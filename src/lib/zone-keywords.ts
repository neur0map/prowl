/**
 * Zone classification keyword lists.
 *
 * Used by elk-adapter.ts to classify clusters into frontend/backend/shared/config/infra zones.
 * Organised into two categories:
 *
 *   1. **Directory signals** — matched against path segments (exact match).
 *   2. **Content keywords** — matched against camelCase-split symbol names (substring match).
 *      These are weighted 2× stronger than directory signals because they reflect what the
 *      code actually does, not just where it lives.
 *
 * "stores" / "store" are intentionally absent from both frontend and backend lists because
 * a store can hold either UI state (sidebar, toast) or API state (supabase, auth).
 */

/* ═══════════════════════════════════════════════════════
 *  DIRECTORY SIGNALS  (matched against path segments)
 * ═══════════════════════════════════════════════════════ */

export const DIR_FRONTEND = new Set([
  // Structural
  'component', 'components',
  'pages', 'page',
  'views', 'view',
  'ui',
  'layout', 'layouts',
  'screens', 'screen',
  'templates', 'template',
  'widgets', 'widget',
  'sections',
  'partials',
  // Framework patterns
  'hooks',
  'composables',
  'directives',
  'mixins',
  'renderer',
  'atoms', 'molecules', 'organisms',  // atomic design
  // Role
  'frontend',
  'client',
  'web',
  'browser',
  'public',
  'static',
  'assets',
  // Styling
  'styles', 'css', 'scss', 'sass', 'less',
  'themes',
  'fonts',
  'icons',
  'images',
  'svg',
]);

export const DIR_BACKEND = new Set([
  // Structural
  'server',
  'api',
  'routes', 'route',
  'controllers', 'controller',
  'services', 'service',
  'handlers', 'handler',
  'middleware', 'middlewares',
  'models', 'model',
  'entities', 'entity',
  'schemas', 'schema',
  'resolvers', 'resolver',
  // Data layer
  'database', 'db',
  'repositories', 'repository',
  'dao',
  'migrations', 'migration',
  'seeders', 'seeds',
  'fixtures',
  // Auth
  'auth', 'authentication', 'authorization',
  'guards', 'guard',
  'policies', 'policy',
  'permissions',
  // Messaging & jobs
  'workers', 'worker',
  'jobs', 'job',
  'queues', 'queue',
  'events', 'listeners',
  'subscribers',
  'cron',
  'tasks',
  // API flavours
  'graphql', 'gql',
  'rest',
  'rpc', 'grpc', 'trpc',
  'websocket', 'ws', 'sockets',
  // External integrations
  'proxy',
  'gateway',
  'webhooks', 'webhook',
  'integrations',
  'connectors',
  // Runtime
  'background',
  'functions',       // serverless functions (e.g. Supabase Edge, Cloudflare Workers)
  'lambda', 'lambdas',
  'edge',
  'backend',
]);

export const DIR_CONFIG = new Set([
  'config', 'configs', 'configuration',
  'settings',
  'env', 'envs',
  'constants',
  'options',
  'preferences',
  'defaults',
  'flags',
  'feature-flags',
  'i18n', 'locales', 'locale', 'lang', 'translations',
]);

export const DIR_INFRA = new Set([
  'deploy', 'deployment', 'deployments',
  'docker',
  'ci', 'cd', 'cicd',
  'infra', 'infrastructure',
  'scripts', 'script',
  'build',
  'tools', 'tooling',
  'devops',
  'terraform', 'tf',
  'k8s', 'kubernetes',
  'helm', 'charts',
  'ansible', 'puppet', 'chef',
  'github', 'gitlab',
  'workflows',
  'pipelines', 'pipeline',
  'monitoring', 'observability',
  'logging', 'logs',
  'telemetry',
]);

export const DIR_DOCS = new Set([
  'docs', 'doc', 'documentation',
  'guides', 'guide',
  'wiki',
  'notes',
  'tutorials', 'tutorial',
  'examples', 'example',
  'samples', 'sample',
  'readme',
  'man', 'manpages',
  'help',
  'faq',
  'api-docs',
  'specifications', 'specs', 'spec',
  'rfcs', 'rfc',
  'adr', 'decisions',
  'changelog',
  'contributing',
]);

/* ═══════════════════════════════════════════════════════
 *  CONTENT KEYWORDS  (matched against symbol name words)
 * ═══════════════════════════════════════════════════════ */

export const CONTENT_BACKEND = [
  // ── Database clients & ORMs ──
  'supabase', 'firebase', 'firestore', 'appwrite',
  'prisma', 'drizzle', 'typeorm', 'sequelize',
  'mongoose', 'mongo', 'mongodb',
  'knex', 'objection', 'bookshelf',
  'redis', 'memcached',
  'postgres', 'mysql', 'sqlite', 'mariadb',
  'dynamo', 'dynamodb', 'cockroach', 'cassandra',
  'elastic', 'elasticsearch', 'algolia', 'meilisearch',
  'kysely', 'mikro',

  // ── Auth & identity ──
  'auth', 'authn', 'authz',
  'login', 'logout',
  'signup', 'signin', 'signout', 'signoff', 'register',
  'password', 'passwd', 'hash',
  'session', 'sessions',
  'token', 'tokens', 'refresh',
  'oauth', 'oidc', 'saml', 'sso',
  'credential', 'credentials',
  'jwt', 'bearer',
  'verify', 'verification',
  'mfa', 'totp', 'otp',
  'permission', 'permissions',
  'role', 'roles',
  'policy', 'policies',
  'clerk', 'nextauth', 'lucia', 'passport',

  // ── API & networking ──
  'endpoint', 'endpoints',
  'webhook', 'webhooks',
  'proxy', 'gateway',
  'graphql', 'mutation', 'resolver',
  'subscription', 'subscriptions',
  'websocket', 'socket',
  'grpc', 'trpc', 'rpc',
  'rest', 'crud',
  'middleware',
  'handler', 'controller',
  'interceptor', 'guard',

  // ── Data operations ──
  'database', 'schema',
  'query', 'queries',
  'migration', 'migrate',
  'seed', 'seeder',
  'repository',
  'aggregate', 'projection',
  'index', 'reindex',
  'backup', 'restore',
  'transaction', 'commit', 'rollback',

  // ── Payments & billing ──
  'payment', 'payments',
  'checkout', 'purchase',
  'stripe', 'paypal', 'braintree', 'paddle', 'lemonsqueezy',
  'billing', 'invoice', 'invoices',
  'subscription', 'plan', 'pricing',
  'refund', 'charge', 'payout',
  'coupon', 'discount',
  'tax', 'vat',

  // ── Email & notifications ──
  'email', 'emails', 'mailer', 'mail',
  'smtp', 'sendgrid', 'resend', 'postmark', 'mailgun', 'ses',
  'notification', 'notifications', 'notify',
  'push', 'sms', 'twilio',
  'alert', 'alerts',

  // ── File & storage ──
  'upload', 'uploads', 'uploader',
  'download', 'downloads',
  'storage', 'blob',
  'bucket', 'buckets',
  'cdn', 'cloudflare', 'cloudfront',
  's3', 'gcs', 'r2',
  'image', 'resize', 'thumbnail',

  // ── AI & ML ──
  'openai', 'anthropic', 'claude', 'gemini', 'cohere', 'replicate',
  'embedding', 'embeddings',
  'vector', 'vectors', 'pinecone', 'qdrant', 'weaviate', 'chroma',
  'llm', 'gpt', 'completion', 'completions',
  'prompt', 'prompts',
  'inference', 'predict', 'prediction',
  'model', 'finetune',
  'rag', 'retrieval',
  'agent', 'chain', 'langchain',
  'tokenize', 'tokenizer',

  // ── Background jobs & workers ──
  'worker', 'workers',
  'cron', 'schedule', 'scheduler',
  'queue', 'enqueue', 'dequeue',
  'job', 'jobs', 'task',
  'batch', 'pipeline',
  'consumer', 'producer',
  'pubsub', 'publish',
  'event', 'emit', 'listener',
  'dispatch', 'bus',

  // ── Server & runtime ──
  'server', 'serve',
  'lambda', 'serverless', 'edge',
  'process', 'spawn', 'exec',
  'cluster', 'replica',
  'health', 'heartbeat', 'ping',
  'rate', 'limit', 'throttle',
  'cache', 'invalidate', 'ttl',
  'encrypt', 'decrypt', 'cipher',
  'secret', 'vault',

  // ── Fetch / HTTP (common in API layers) ──
  'fetch', 'request', 'response',
  'axios', 'ky', 'got', 'superagent',
  'http', 'https',
  'header', 'headers',
  'cookie', 'cookies',
  'cors',
  'csrf', 'xsrf',
];

export const CONTENT_FRONTEND = [
  // ── Components & rendering ──
  'component', 'components',
  'render', 'renderer',
  'mount', 'unmount', 'hydrate',
  'slot', 'slots',
  'portal', 'teleport',
  'suspense', 'fallback',
  'fragment',
  'virtual', 'vdom', 'vnode',
  'jsx', 'tsx',

  // ── Layout & containers ──
  'sidebar', 'sidepanel',
  'panel', 'panels',
  'drawer',
  'navbar', 'nav', 'navigation',
  'toolbar',
  'header', 'footer',
  'appbar', 'statusbar', 'tabbar',
  'grid', 'flex', 'stack',
  'container', 'wrapper',
  'section', 'card',
  'divider', 'spacer',
  'breadcrumb', 'breadcrumbs',

  // ── UI primitives ──
  'modal', 'dialog',
  'popup', 'popover', 'overlay',
  'toast', 'snackbar',
  'tooltip',
  'dropdown', 'menu', 'context',
  'accordion', 'collapsible', 'expandable',
  'carousel', 'slider', 'swiper',
  'tab', 'tabs',
  'badge', 'chip', 'tag', 'pill',
  'avatar',
  'skeleton', 'shimmer', 'placeholder',
  'spinner', 'loader', 'loading',
  'progress', 'progressbar',
  'stepper', 'wizard',
  'pagination', 'paginator',

  // ── Forms & inputs ──
  'input', 'textarea',
  'select', 'picker', 'datepicker', 'timepicker', 'colorpicker',
  'checkbox', 'radio', 'toggle', 'switch',
  'button', 'btn',
  'label',
  'field', 'fieldset',
  'validate', 'validation', 'validator',
  'autocomplete', 'combobox', 'typeahead',
  'search', 'searchbar',
  'upload', 'dropzone',
  'slider', 'range',
  'rating', 'star',

  // ── Animation & transitions ──
  'animation', 'animate', 'animated',
  'transition', 'transitions',
  'motion', 'framer',
  'spring', 'tween', 'ease', 'easing',
  'keyframe', 'keyframes',
  'fade', 'slide', 'scale', 'rotate', 'flip',
  'enter', 'leave', 'appear',
  'gsap', 'lottie',

  // ── Interaction & events ──
  'scroll', 'scrollable', 'scrollbar',
  'drag', 'draggable', 'droppable', 'sortable',
  'resize', 'resizable',
  'viewport', 'breakpoint',
  'clamp', 'overflow',
  'hover', 'hovered',
  'click', 'dblclick', 'longpress',
  'focus', 'blur', 'focustrap',
  'keydown', 'keyup', 'keypress', 'shortcut', 'hotkey',
  'swipe', 'pinch', 'gesture', 'touch',
  'intersect', 'intersection', 'observer',
  'clipboard', 'copy', 'paste',

  // ── Styling & theming ──
  'theme', 'themes', 'theming',
  'darkmode', 'lightmode', 'colorscheme',
  'style', 'styles', 'styled',
  'classname', 'classnames', 'clsx',
  'tailwind', 'css', 'scss', 'sass',
  'color', 'palette', 'swatch',
  'font', 'typography',
  'shadow', 'elevation',
  'border', 'radius', 'rounded',
  'opacity', 'backdrop', 'blur',
  'gradient', 'pattern',
  'responsive', 'breakpoints', 'media',
  'spacing', 'padding', 'margin', 'gap',
  'zindex', 'layer',

  // ── Routing & navigation ──
  'route', 'router', 'routing',
  'navigate', 'navigation',
  'redirect', 'history',
  'link', 'href',
  'page', 'pages',
  'layout', 'layouts',
  'outlet',
  'param', 'params', 'query',

  // ── State & reactivity ──
  'useState', 'useEffect', 'useRef', 'useMemo', 'useCallback',
  'useContext', 'useReducer',
  'reactive', 'ref', 'computed', 'watch', 'watchEffect',
  'signal', 'signals',
  'observable', 'subscribe',
  'atom', 'selector',
  'writable', 'readable', 'derived',

  // ── Canvas & visualization ──
  'canvas', 'webgl',
  'svg', 'path',
  'chart', 'graph', 'plot',
  'map', 'marker', 'popup',
  'legend', 'axis', 'tooltip',
  'icon', 'icons',
  'emoji',
  'illustration',

  // ── Accessibility ──
  'aria', 'a11y', 'accessibility',
  'screenreader', 'sr',
  'tabindex',
  'announce', 'live',
];

/* ═══════════════════════════════════════════════════════
 *  FRONTEND LANGUAGES  (file-extension-based)
 * ═══════════════════════════════════════════════════════ */

export const FRONTEND_LANGS = new Set([
  'CSS', 'HTML', 'Vue', 'Svelte',
]);
