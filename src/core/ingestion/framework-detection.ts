/**
 * Path-driven framework detection.
 * Returns a scoring multiplier when the file path matches
 * a known web-framework convention, or null otherwise.
 */

export interface FrameworkHint {
  framework: string;
  entryPointMultiplier: number;
  reason: string;
}

interface Rule {
  match: (path: string, filename: string) => boolean;
  hint: FrameworkHint;
}

const RULES: Rule[] = [
  /* ── Next.js ──────────────────────────────────────── */
  {
    match: p =>
      p.includes('/pages/') && !p.includes('/_') && !p.includes('/api/') &&
      /\.(tsx|ts|jsx|js)$/.test(p),
    hint: { framework: 'nextjs-pages', entryPointMultiplier: 3.0, reason: 'nextjs-page' },
  },
  {
    match: p => p.includes('/app/') && /page\.(tsx|ts|jsx|js)$/.test(p),
    hint: { framework: 'nextjs-app', entryPointMultiplier: 3.0, reason: 'nextjs-app-page' },
  },
  {
    match: p =>
      p.includes('/pages/api/') ||
      (p.includes('/app/') && p.includes('/api/') && p.endsWith('route.ts')),
    hint: { framework: 'nextjs-api', entryPointMultiplier: 3.0, reason: 'nextjs-api-route' },
  },
  {
    match: p => p.includes('/app/') && /layout\.(tsx|ts)$/.test(p),
    hint: { framework: 'nextjs-app', entryPointMultiplier: 2.0, reason: 'nextjs-layout' },
  },

  /* ── Express / Node ───────────────────────────────── */
  {
    match: p => p.includes('/routes/') && /\.(ts|js)$/.test(p),
    hint: { framework: 'express', entryPointMultiplier: 2.5, reason: 'routes-folder' },
  },

  /* ── MVC controllers ──────────────────────────────── */
  {
    match: p => p.includes('/controllers/') && /\.(ts|js)$/.test(p),
    hint: { framework: 'mvc', entryPointMultiplier: 2.5, reason: 'controllers-folder' },
  },

  /* ── Handler directories ──────────────────────────── */
  {
    match: p => p.includes('/handlers/') && /\.(ts|js)$/.test(p),
    hint: { framework: 'handlers', entryPointMultiplier: 2.5, reason: 'handlers-folder' },
  },

  /* ── React components ─────────────────────────────── */
  {
    match: (p, f) =>
      (p.includes('/components/') || p.includes('/views/')) &&
      /\.(tsx|jsx)$/.test(p) && /^[A-Z]/.test(f),
    hint: { framework: 'react', entryPointMultiplier: 1.5, reason: 'react-component' },
  },

  /* ── Django ───────────────────────────────────────── */
  {
    match: p => p.endsWith('views.py'),
    hint: { framework: 'django', entryPointMultiplier: 3.0, reason: 'django-views' },
  },
  {
    match: p => p.endsWith('urls.py'),
    hint: { framework: 'django', entryPointMultiplier: 2.0, reason: 'django-urls' },
  },

  /* ── FastAPI / Flask ──────────────────────────────── */
  {
    match: p =>
      (p.includes('/routers/') || p.includes('/endpoints/') || p.includes('/routes/')) &&
      p.endsWith('.py'),
    hint: { framework: 'fastapi', entryPointMultiplier: 2.5, reason: 'api-routers' },
  },
  {
    match: p => p.includes('/api/') && p.endsWith('.py') && !p.endsWith('__init__.py'),
    hint: { framework: 'python-api', entryPointMultiplier: 2.0, reason: 'api-folder' },
  },

  /* ── Spring Boot ──────────────────────────────────── */
  {
    match: p =>
      (p.includes('/controller/') || p.includes('/controllers/')) && p.endsWith('.java'),
    hint: { framework: 'spring', entryPointMultiplier: 3.0, reason: 'spring-controller' },
  },
  {
    match: p => p.endsWith('controller.java'),
    hint: { framework: 'spring', entryPointMultiplier: 3.0, reason: 'spring-controller-file' },
  },
  {
    match: p =>
      (p.includes('/service/') || p.includes('/services/')) && p.endsWith('.java'),
    hint: { framework: 'java-service', entryPointMultiplier: 1.8, reason: 'java-service' },
  },

  /* ── ASP.NET ──────────────────────────────────────── */
  {
    match: p => p.includes('/controllers/') && p.endsWith('.cs'),
    hint: { framework: 'aspnet', entryPointMultiplier: 3.0, reason: 'aspnet-controller' },
  },
  {
    match: p => p.endsWith('controller.cs'),
    hint: { framework: 'aspnet', entryPointMultiplier: 3.0, reason: 'aspnet-controller-file' },
  },
  {
    match: p => p.includes('/pages/') && p.endsWith('.razor'),
    hint: { framework: 'blazor', entryPointMultiplier: 2.5, reason: 'blazor-page' },
  },

  /* ── Go ───────────────────────────────────────────── */
  {
    match: p => (p.includes('/handlers/') || p.includes('/handler/')) && p.endsWith('.go'),
    hint: { framework: 'go-http', entryPointMultiplier: 2.5, reason: 'go-handlers' },
  },
  {
    match: p => p.includes('/routes/') && p.endsWith('.go'),
    hint: { framework: 'go-http', entryPointMultiplier: 2.5, reason: 'go-routes' },
  },
  {
    match: p => p.includes('/controllers/') && p.endsWith('.go'),
    hint: { framework: 'go-mvc', entryPointMultiplier: 2.5, reason: 'go-controller' },
  },
  {
    match: p => p.endsWith('/main.go') || (p.endsWith('/cmd/') && p.endsWith('.go')),
    hint: { framework: 'go', entryPointMultiplier: 3.0, reason: 'go-main' },
  },

  /* ── Rust ──────────────────────────────────────────── */
  {
    match: p => (p.includes('/handlers/') || p.includes('/routes/')) && p.endsWith('.rs'),
    hint: { framework: 'rust-web', entryPointMultiplier: 2.5, reason: 'rust-handlers' },
  },
  {
    match: p => p.endsWith('/main.rs'),
    hint: { framework: 'rust', entryPointMultiplier: 3.0, reason: 'rust-main' },
  },
  {
    match: p => p.includes('/bin/') && p.endsWith('.rs'),
    hint: { framework: 'rust', entryPointMultiplier: 2.5, reason: 'rust-bin' },
  },

  /* ── C / C++ ──────────────────────────────────────── */
  {
    match: p => p.endsWith('/main.c') || p.endsWith('/main.cpp') || p.endsWith('/main.cc'),
    hint: { framework: 'c-cpp', entryPointMultiplier: 3.0, reason: 'c-main' },
  },
  {
    match: p => p.includes('/src/') && (p.endsWith('/app.c') || p.endsWith('/app.cpp')),
    hint: { framework: 'c-cpp', entryPointMultiplier: 2.5, reason: 'c-app' },
  },

  /* ── Generic API index ────────────────────────────── */
  {
    match: p =>
      p.includes('/api/') &&
      (p.endsWith('/index.ts') || p.endsWith('/index.js') || p.endsWith('/__init__.py')),
    hint: { framework: 'api', entryPointMultiplier: 1.8, reason: 'api-index' },
  },
];

/** Evaluate a file path against the rule table. */
export function detectFrameworkFromPath(filePath: string): FrameworkHint | null {
  let norm = filePath.toLowerCase().replace(/\\/g, '/');
  if (!norm.startsWith('/')) norm = '/' + norm;

  const lastSlash = norm.lastIndexOf('/');
  const fname = lastSlash >= 0 ? norm.substring(lastSlash + 1) : norm;

  for (const r of RULES) {
    if (r.match(norm, fname)) return { ...r.hint };
  }
  return null;
}

/** Decorator/annotation patterns reserved for future AST-level detection. */
export const FRAMEWORK_AST_PATTERNS: Record<string, string[]> = {
  nestjs: ['@Controller', '@Get', '@Post', '@Put', '@Delete', '@Patch'],
  express: ['app.get', 'app.post', 'app.put', 'app.delete', 'router.get', 'router.post'],
  fastapi: ['@app.get', '@app.post', '@app.put', '@app.delete', '@router.get'],
  flask: ['@app.route', '@blueprint.route'],
  spring: ['@RestController', '@Controller', '@GetMapping', '@PostMapping', '@RequestMapping'],
  jaxrs: ['@Path', '@GET', '@POST', '@PUT', '@DELETE'],
  aspnet: ['[ApiController]', '[HttpGet]', '[HttpPost]', '[Route]'],
  'go-http': ['http.Handler', 'http.HandlerFunc', 'ServeHTTP'],
  actix: ['#[get', '#[post', '#[put', '#[delete'],
  axum: ['Router::new'],
  rocket: ['#[get', '#[post'],
};
