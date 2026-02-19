/**
 * Framework Detection
 * 
 * Detects frameworks from file path patterns and provides entry point multipliers.
 * This enables framework-aware entry point scoring.
 * 
 * DESIGN: Returns null for unknown frameworks, which causes a 1.0 multiplier
 * (no bonus, no penalty) - same behavior as before this feature.
 */

// ============================================================================
// TYPES
// ============================================================================

export interface FrameworkHint {
  framework: string;
  entryPointMultiplier: number;
  reason: string;
}

// ============================================================================
// PATH-BASED FRAMEWORK DETECTION
// ============================================================================

/**
 * Detect framework from file path patterns
 * 
 * This provides entry point multipliers based on well-known framework conventions.
 * Returns null if no framework pattern is detected (falls back to 1.0 multiplier).
 */
export function detectFrameworkFromPath(filePath: string): FrameworkHint | null {
  // Normalize path separators and ensure leading slash for consistent matching
  let p = filePath.toLowerCase().replace(/\\/g, '/');
  if (!p.startsWith('/')) {
    p = '/' + p;  // Add leading slash so patterns like '/app/' match 'app/...'
  }
  
  // ========== JAVASCRIPT / TYPESCRIPT FRAMEWORKS ==========
  
  // Next.js - Pages Router (high confidence)
  if (p.includes('/pages/') && !p.includes('/_') && !p.includes('/api/')) {
    if (p.endsWith('.tsx') || p.endsWith('.ts') || p.endsWith('.jsx') || p.endsWith('.js')) {
      return { framework: 'nextjs-pages', entryPointMultiplier: 3.0, reason: 'nextjs-page' };
    }
  }
  
  // Next.js - App Router (page.tsx files)
  if (p.includes('/app/') && (
    p.endsWith('page.tsx') || p.endsWith('page.ts') || 
    p.endsWith('page.jsx') || p.endsWith('page.js')
  )) {
    return { framework: 'nextjs-app', entryPointMultiplier: 3.0, reason: 'nextjs-app-page' };
  }
  
  // Next.js - API Routes
  if (p.includes('/pages/api/') || (p.includes('/app/') && p.includes('/api/') && p.endsWith('route.ts'))) {
    return { framework: 'nextjs-api', entryPointMultiplier: 3.0, reason: 'nextjs-api-route' };
  }
  
  // Next.js - Layout files (moderate - they're entry-ish but not the main entry)
  if (p.includes('/app/') && (p.endsWith('layout.tsx') || p.endsWith('layout.ts'))) {
    return { framework: 'nextjs-app', entryPointMultiplier: 2.0, reason: 'nextjs-layout' };
  }
  
  // Express / Node.js routes
  if (p.includes('/routes/') && (p.endsWith('.ts') || p.endsWith('.js'))) {
    return { framework: 'express', entryPointMultiplier: 2.5, reason: 'routes-folder' };
  }
  
  // Generic controllers (MVC pattern)
  if (p.includes('/controllers/') && (p.endsWith('.ts') || p.endsWith('.js'))) {
    return { framework: 'mvc', entryPointMultiplier: 2.5, reason: 'controllers-folder' };
  }
  
  // Generic handlers
  if (p.includes('/handlers/') && (p.endsWith('.ts') || p.endsWith('.js'))) {
    return { framework: 'handlers', entryPointMultiplier: 2.5, reason: 'handlers-folder' };
  }
  
  // React components (lower priority - not all are entry points)
  if ((p.includes('/components/') || p.includes('/views/')) && 
      (p.endsWith('.tsx') || p.endsWith('.jsx'))) {
    // Only boost if PascalCase filename (likely a component, not util)
    const fileName = p.split('/').pop() || '';
    if (/^[A-Z]/.test(fileName)) {
      return { framework: 'react', entryPointMultiplier: 1.5, reason: 'react-component' };
    }
  }
  
  // ========== PYTHON FRAMEWORKS ==========
  
  // Django views (high confidence)
  if (p.endsWith('views.py')) {
    return { framework: 'django', entryPointMultiplier: 3.0, reason: 'django-views' };
  }
  
  // Django URL configs
  if (p.endsWith('urls.py')) {
    return { framework: 'django', entryPointMultiplier: 2.0, reason: 'django-urls' };
  }
  
  // FastAPI / Flask routers
  if ((p.includes('/routers/') || p.includes('/endpoints/') || p.includes('/routes/')) && 
      p.endsWith('.py')) {
    return { framework: 'fastapi', entryPointMultiplier: 2.5, reason: 'api-routers' };
  }
  
  // Python API folder
  if (p.includes('/api/') && p.endsWith('.py') && !p.endsWith('__init__.py')) {
    return { framework: 'python-api', entryPointMultiplier: 2.0, reason: 'api-folder' };
  }
  
  // ========== JAVA FRAMEWORKS ==========
  
  // Spring Boot controllers
  if ((p.includes('/controller/') || p.includes('/controllers/')) && p.endsWith('.java')) {
    return { framework: 'spring', entryPointMultiplier: 3.0, reason: 'spring-controller' };
  }
  
  // Spring Boot - files ending in Controller.java
  if (p.endsWith('controller.java')) {
    return { framework: 'spring', entryPointMultiplier: 3.0, reason: 'spring-controller-file' };
  }
  
  // Java service layer (often entry points for business logic)
  if ((p.includes('/service/') || p.includes('/services/')) && p.endsWith('.java')) {
    return { framework: 'java-service', entryPointMultiplier: 1.8, reason: 'java-service' };
  }
  
  // ========== C# / .NET FRAMEWORKS ==========
  
  // ASP.NET Controllers
  if (p.includes('/controllers/') && p.endsWith('.cs')) {
    return { framework: 'aspnet', entryPointMultiplier: 3.0, reason: 'aspnet-controller' };
  }
  
  // ASP.NET - files ending in Controller.cs
  if (p.endsWith('controller.cs')) {
    return { framework: 'aspnet', entryPointMultiplier: 3.0, reason: 'aspnet-controller-file' };
  }
  
  // Blazor pages
  if (p.includes('/pages/') && p.endsWith('.razor')) {
    return { framework: 'blazor', entryPointMultiplier: 2.5, reason: 'blazor-page' };
  }
  
  // ========== GO FRAMEWORKS ==========
  
  // Go handlers
  if ((p.includes('/handlers/') || p.includes('/handler/')) && p.endsWith('.go')) {
    return { framework: 'go-http', entryPointMultiplier: 2.5, reason: 'go-handlers' };
  }
  
  // Go routes
  if (p.includes('/routes/') && p.endsWith('.go')) {
    return { framework: 'go-http', entryPointMultiplier: 2.5, reason: 'go-routes' };
  }
  
  // Go controllers
  if (p.includes('/controllers/') && p.endsWith('.go')) {
    return { framework: 'go-mvc', entryPointMultiplier: 2.5, reason: 'go-controller' };
  }
  
  // Go main.go files (THE entry point)
  if (p.endsWith('/main.go') || p.endsWith('/cmd/') && p.endsWith('.go')) {
    return { framework: 'go', entryPointMultiplier: 3.0, reason: 'go-main' };
  }
  
  // ========== RUST FRAMEWORKS ==========
  
  // Rust handlers/routes
  if ((p.includes('/handlers/') || p.includes('/routes/')) && p.endsWith('.rs')) {
    return { framework: 'rust-web', entryPointMultiplier: 2.5, reason: 'rust-handlers' };
  }
  
  // Rust main.rs (THE entry point)
  if (p.endsWith('/main.rs')) {
    return { framework: 'rust', entryPointMultiplier: 3.0, reason: 'rust-main' };
  }
  
  // Rust bin folder (executables)
  if (p.includes('/bin/') && p.endsWith('.rs')) {
    return { framework: 'rust', entryPointMultiplier: 2.5, reason: 'rust-bin' };
  }
  
  // ========== C / C++ ==========
  
  // C/C++ main files
  if (p.endsWith('/main.c') || p.endsWith('/main.cpp') || p.endsWith('/main.cc')) {
    return { framework: 'c-cpp', entryPointMultiplier: 3.0, reason: 'c-main' };
  }
  
  // C/C++ src folder entry points (if named specifically)
  if ((p.includes('/src/') && (p.endsWith('/app.c') || p.endsWith('/app.cpp')))) {
    return { framework: 'c-cpp', entryPointMultiplier: 2.5, reason: 'c-app' };
  }
  
  // ========== GENERIC PATTERNS ==========
  
  // Any language: index files in API folders
  if (p.includes('/api/') && (
    p.endsWith('/index.ts') || p.endsWith('/index.js') || 
    p.endsWith('/__init__.py')
  )) {
    return { framework: 'api', entryPointMultiplier: 1.8, reason: 'api-index' };
  }
  
  // No framework detected - return null for graceful fallback (1.0 multiplier)
  return null;
}

// ============================================================================
// FUTURE: AST-BASED PATTERNS (for Phase 3)
// ============================================================================

/**
 * Patterns that indicate entry points within code (for future AST-based detection)
 * These would require parsing decorators/annotations in the code itself.
 */
export const FRAMEWORK_AST_PATTERNS = {
  // JavaScript/TypeScript decorators
  'nestjs': ['@Controller', '@Get', '@Post', '@Put', '@Delete', '@Patch'],
  'express': ['app.get', 'app.post', 'app.put', 'app.delete', 'router.get', 'router.post'],
  
  // Python decorators
  'fastapi': ['@app.get', '@app.post', '@app.put', '@app.delete', '@router.get'],
  'flask': ['@app.route', '@blueprint.route'],
  
  // Java annotations
  'spring': ['@RestController', '@Controller', '@GetMapping', '@PostMapping', '@RequestMapping'],
  'jaxrs': ['@Path', '@GET', '@POST', '@PUT', '@DELETE'],
  
  // C# attributes
  'aspnet': ['[ApiController]', '[HttpGet]', '[HttpPost]', '[Route]'],
  
  // Go patterns (function signatures)
  'go-http': ['http.Handler', 'http.HandlerFunc', 'ServeHTTP'],
  
  // Rust macros
  'actix': ['#[get', '#[post', '#[put', '#[delete'],
  'axum': ['Router::new'],
  'rocket': ['#[get', '#[post'],
};
