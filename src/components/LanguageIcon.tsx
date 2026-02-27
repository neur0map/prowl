import { getFileIcon } from '../lib/file-icons'

interface LanguageIconProps {
  filename: string
  size?: number
  className?: string
}

/* ── Badge helper: colored rounded rect with bold text ─────────── */
function Badge({ bg, fg = '#fff', text, sz }: { bg: string; fg?: string; text: string; sz: number }) {
  const fontSize = text.length > 2 ? 5.8 : text.length > 1 ? 7 : 8.5
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
      <rect x="0.5" y="0.5" width="15" height="15" rx="3.5" fill={bg} />
      <text
        x="8" y="11.5"
        textAnchor="middle"
        fill={fg}
        fontSize={fontSize}
        fontWeight="700"
        fontFamily="system-ui, -apple-system, sans-serif"
      >
        {text}
      </text>
    </svg>
  )
}

/* ── React atom (JSX / TSX) ───────────────────────────────────── */
function ReactAtom({ sz, color = '#61DAFB' }: { sz: number; color?: string }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <circle cx="8" cy="8" r="1.4" fill={color} />
      <ellipse cx="8" cy="8" rx="6.5" ry="2.4" stroke={color} strokeWidth="0.75" fill="none" />
      <ellipse cx="8" cy="8" rx="6.5" ry="2.4" stroke={color} strokeWidth="0.75" fill="none" transform="rotate(60 8 8)" />
      <ellipse cx="8" cy="8" rx="6.5" ry="2.4" stroke={color} strokeWidth="0.75" fill="none" transform="rotate(120 8 8)" />
    </svg>
  )
}

/* ── Python (two snakes) ──────────────────────────────────────── */
function PythonLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M7.8 1.2C5.7 1.2 5.9 2.1 5.9 2.1L5.9 3.3H8V3.8H4.1C4.1 3.8 2 3.5 2 6.1S3.7 8.5 3.7 8.5H4.9V7.2C4.9 7.2 4.8 5.4 6.7 5.4H9C10.6 5.4 10.8 3.6 10.8 3.6V2C10.8 2 11.1 1.2 7.8 1.2ZM6.5 2C6.8 2 7 2.2 7 2.5S6.8 3 6.5 3S6 2.8 6 2.5S6.2 2 6.5 2Z" fill="#3776AB"/>
      <path d="M8.2 14.8C10.3 14.8 10.1 13.9 10.1 13.9L10.1 12.7H8V12.2H11.9C11.9 12.2 14 12.5 14 9.9S12.3 7.5 12.3 7.5H11.1V8.8C11.1 8.8 11.2 10.6 9.3 10.6H7C5.4 10.6 5.2 12.4 5.2 12.4V14C5.2 14 4.9 14.8 8.2 14.8ZM9.5 14C9.2 14 9 13.8 9 13.5S9.2 13 9.5 13S10 13.2 10 13.5S9.8 14 9.5 14Z" fill="#FFD43B"/>
    </svg>
  )
}

/* ── Vue chevrons ─────────────────────────────────────────────── */
function VueLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M12.8 1.5H10.2L8 5.2L5.8 1.5H3.2L8 10L12.8 1.5Z" fill="#41B883"/>
      <path d="M10.2 1.5H8.8L8 3L7.2 1.5H5.8L8 5.2L10.2 1.5Z" fill="#35495E"/>
    </svg>
  )
}

/* ── Svelte S ─────────────────────────────────────────────────── */
function SvelteLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M12.5 2.8C11.3 1.1 9.1 0.7 7.3 1.7L4.7 3.5C4 3.9 3.5 4.7 3.3 5.5C3.2 6.2 3.3 6.8 3.5 7.4C3.3 7.7 3.2 8.1 3.1 8.5C3 9.4 3.3 10.3 3.9 11C5.1 12.7 7.3 13.1 9.1 12.1L11.7 10.3C12.4 9.9 12.9 9.1 13.1 8.3C13.2 7.6 13.1 7 12.9 6.4C13.1 6.1 13.2 5.7 13.3 5.3C13.4 4.4 13.1 3.5 12.5 2.8Z" fill="#FF3E00"/>
      <path d="M7.5 11.2C6.7 11.4 5.9 11 5.4 10.4C5.1 10 5 9.5 5.1 9L5.2 8.7L5.5 8.9C5.8 9.1 6.2 9.2 6.6 9.3L6.7 9.3C6.7 9.4 6.8 9.5 6.8 9.6C6.9 9.8 7.1 9.9 7.3 9.8L9.9 8C10.1 7.9 10.1 7.7 10.1 7.6C10.1 7.4 10 7.2 9.8 7.1C9.6 6.9 9.2 6.8 8.9 6.9L8 7.5C7.1 8 5.9 7.8 5.2 7C4.9 6.6 4.7 6.1 4.8 5.6C4.9 5.1 5.2 4.7 5.6 4.4L8.2 2.6C8.4 2.5 8.6 2.4 8.8 2.4C9.6 2.2 10.4 2.4 10.9 2.9C11.2 3.3 11.3 3.8 11.2 4.3L11.1 4.6L10.8 4.4C10.5 4.2 10.1 4.1 9.7 4L9.6 4C9.6 3.9 9.5 3.8 9.5 3.7C9.4 3.5 9.2 3.4 9 3.5L6.4 5.3C6.2 5.4 6.2 5.6 6.2 5.7C6.2 5.9 6.3 6.1 6.5 6.2C6.7 6.4 7.1 6.5 7.4 6.4L8.3 5.8C9.2 5.3 10.4 5.5 11.1 6.3C11.4 6.7 11.6 7.2 11.5 7.7C11.4 8.2 11.1 8.6 10.7 8.9L8.1 10.7C7.9 10.8 7.7 11.1 7.5 11.2Z" fill="#fff"/>
    </svg>
  )
}

/* ── Docker whale ─────────────────────────────────────────────── */
function DockerLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M9 6.5H10.5V8H9V6.5ZM7 6.5H8.5V8H7V6.5ZM5 6.5H6.5V8H5V6.5ZM7 4.5H8.5V6H7V4.5ZM5 4.5H6.5V6H5V4.5ZM3 6.5H4.5V8H3V6.5ZM7 2.5H8.5V4H7V2.5Z" fill="#2496ED"/>
      <path d="M14.5 7.8C14.2 6.8 13.3 6.5 13.3 6.5C13.4 5.8 13 5.3 13 5.3L12.5 5.5C12.1 5.1 11.5 5 11.5 5V5.5C11.2 5.3 10.8 5.3 10.8 5.3V6H11V6.5H1.5C1.5 6.5 1.3 9.5 4 11C4.7 11.4 5.6 11.7 6.7 11.7C10 11.7 12.5 10 14 8.5C14.5 8 14.5 7.8 14.5 7.8Z" fill="#2496ED"/>
    </svg>
  )
}

/* ── Git branch ───────────────────────────────────────────────── */
function GitLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M14.2 7.3L8.7 1.8C8.3 1.4 7.7 1.4 7.3 1.8L5.9 3.2L7.6 4.9C8 4.8 8.5 4.9 8.8 5.2C9.1 5.5 9.2 6 9.1 6.4L10.7 8C11.1 7.9 11.6 8 11.9 8.3C12.3 8.7 12.3 9.4 11.9 9.8C11.5 10.2 10.8 10.2 10.4 9.8C10.1 9.5 10 8.9 10.2 8.5L8.7 7V10.5C8.8 10.6 8.9 10.6 9 10.7C9.4 11.1 9.4 11.8 9 12.2C8.6 12.6 7.9 12.6 7.5 12.2C7.1 11.8 7.1 11.1 7.5 10.7C7.6 10.6 7.7 10.5 7.8 10.5V6.9C7.7 6.8 7.6 6.8 7.5 6.7C7.2 6.4 7.1 5.9 7.2 5.5L5.5 3.8L1.8 7.3C1.4 7.7 1.4 8.3 1.8 8.7L7.3 14.2C7.7 14.6 8.3 14.6 8.7 14.2L14.2 8.7C14.6 8.3 14.6 7.7 14.2 7.3Z" fill="#F05032"/>
    </svg>
  )
}

/* ── Markdown M↓ ──────────────────────────────────────────────── */
function MarkdownLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <rect x="1" y="3" width="14" height="10" rx="2" stroke="#7EAAC4" strokeWidth="1" fill="none"/>
      <path d="M3.5 10V6L5.5 8.5L7.5 6V10" stroke="#7EAAC4" strokeWidth="1.1" strokeLinecap="round" strokeLinejoin="round" fill="none"/>
      <path d="M10 8.5L12 10.5L14 8.5" stroke="#7EAAC4" strokeWidth="1.1" strokeLinecap="round" strokeLinejoin="round" fill="none" transform="translate(-1.5, -2.5)"/>
    </svg>
  )
}

/* ── Shell terminal >_ ────────────────────────────────────────── */
function ShellIcon({ sz, color = '#8AAA78' }: { sz: number; color?: string }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <rect x="1" y="2.5" width="14" height="11" rx="2.5" stroke={color} strokeWidth="0.9" fill="none"/>
      <path d="M4 7L6.5 8.5L4 10" stroke={color} strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round" fill="none"/>
      <line x1="8" y1="10" x2="11.5" y2="10" stroke={color} strokeWidth="1.2" strokeLinecap="round"/>
    </svg>
  )
}

/* ── JSON braces ──────────────────────────────────────────────── */
function JsonIcon({ sz, color = '#C4907A' }: { sz: number; color?: string }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <text x="8" y="12" textAnchor="middle" fill={color} fontSize="11" fontWeight="600" fontFamily="monospace">
        {'{}'}
      </text>
    </svg>
  )
}

/* ── Rust gear ────────────────────────────────────────────────── */
function RustLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <circle cx="8" cy="8" r="5" stroke="#D0884C" strokeWidth="1.2" fill="none"/>
      <circle cx="8" cy="8" r="2" fill="#D0884C"/>
      {/* gear teeth */}
      {[0, 45, 90, 135, 180, 225, 270, 315].map(angle => (
        <line
          key={angle}
          x1="8" y1="2.2" x2="8" y2="3.8"
          stroke="#D0884C" strokeWidth="1.4" strokeLinecap="round"
          transform={`rotate(${angle} 8 8)`}
        />
      ))}
    </svg>
  )
}

/* ── Ruby gem ─────────────────────────────────────────────────── */
function RubyLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <polygon points="8,2 13,6 11,14 5,14 3,6" fill="#CC342D"/>
      <polygon points="8,2 10,6 8,14 6,6" fill="#E44D26" opacity="0.5"/>
    </svg>
  )
}

/* ── Swift bird ───────────────────────────────────────────────── */
function SwiftLogo({ sz }: { sz: number }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <path d="M12.5 3C12.5 3 9.5 6 6 7.5C7.5 6.5 10 3.5 10 3.5C7.5 5.5 4 7 3 7.2C4 6 8 2.5 8 2.5C5.5 4 2.5 5.5 2 5.5C2 5.5 3 8 6 10C4.5 10 3 9.5 2 8.5C2 8.5 3.5 12 7.5 13C9.5 13.4 12 12.5 13.5 10.5C14.5 9 14.5 6 12.5 3Z" fill="#F05138"/>
    </svg>
  )
}

/* ═══════════════════════════════════════════════════════════════
   EXTENSION → ICON MAPPING
═══════════════════════════════════════════════════════════════ */

type IconRenderer = (sz: number) => JSX.Element

const EXT_ICONS: Record<string, IconRenderer> = {
  // JavaScript
  js:  (sz) => <Badge bg="#F7DF1E" fg="#1a1a1a" text="JS" sz={sz} />,
  mjs: (sz) => <Badge bg="#F7DF1E" fg="#1a1a1a" text="JS" sz={sz} />,
  cjs: (sz) => <Badge bg="#F7DF1E" fg="#1a1a1a" text="JS" sz={sz} />,

  // TypeScript
  ts:  (sz) => <Badge bg="#3178C6" text="TS" sz={sz} />,
  mts: (sz) => <Badge bg="#3178C6" text="TS" sz={sz} />,
  cts: (sz) => <Badge bg="#3178C6" text="TS" sz={sz} />,

  // React
  jsx: (sz) => <ReactAtom sz={sz} />,
  tsx: (sz) => <ReactAtom sz={sz} color="#3178C6" />,

  // Web
  html: (sz) => <Badge bg="#E44D26" text="H" sz={sz} />,
  htm:  (sz) => <Badge bg="#E44D26" text="H" sz={sz} />,
  css:  (sz) => <Badge bg="#1572B6" text="#" sz={sz} />,
  scss: (sz) => <Badge bg="#CC6699" text="S" sz={sz} />,
  sass: (sz) => <Badge bg="#CC6699" text="S" sz={sz} />,
  less: (sz) => <Badge bg="#1D365D" text="L" sz={sz} />,
  vue:  (sz) => <VueLogo sz={sz} />,
  svelte: (sz) => <SvelteLogo sz={sz} />,

  // Systems
  rs: (sz) => <RustLogo sz={sz} />,
  go: (sz) => <Badge bg="#00ADD8" text="Go" sz={sz} />,
  c:  (sz) => <Badge bg="#A8B9CC" fg="#1a1a1a" text="C" sz={sz} />,
  h:  (sz) => <Badge bg="#A8B9CC" fg="#1a1a1a" text="H" sz={sz} />,
  cpp: (sz) => <Badge bg="#00599C" text="++" sz={sz} />,
  cc:  (sz) => <Badge bg="#00599C" text="++" sz={sz} />,
  hpp: (sz) => <Badge bg="#00599C" text="++" sz={sz} />,
  zig: (sz) => <Badge bg="#F7A41D" fg="#1a1a1a" text="Z" sz={sz} />,

  // JVM
  java:  (sz) => <Badge bg="#ED8B00" text="Jv" sz={sz} />,
  kt:    (sz) => <Badge bg="#7F52FF" text="Kt" sz={sz} />,
  kts:   (sz) => <Badge bg="#7F52FF" text="Kt" sz={sz} />,
  scala: (sz) => <Badge bg="#DC322F" text="Sc" sz={sz} />,
  groovy: (sz) => <Badge bg="#4298B8" text="Gr" sz={sz} />,
  clj:   (sz) => <Badge bg="#5881D8" text="λ" sz={sz} />,

  // .NET
  cs: (sz) => <Badge bg="#68217A" text="C#" sz={sz} />,
  fs: (sz) => <Badge bg="#378BBA" text="F#" sz={sz} />,
  vb: (sz) => <Badge bg="#68217A" text="VB" sz={sz} />,

  // Scripting
  py:  (sz) => <PythonLogo sz={sz} />,
  pyi: (sz) => <PythonLogo sz={sz} />,
  rb:  (sz) => <RubyLogo sz={sz} />,
  php: (sz) => <Badge bg="#777BB4" text="P" sz={sz} />,
  lua: (sz) => <Badge bg="#000080" text="L" sz={sz} />,
  r:   (sz) => <Badge bg="#276DC3" text="R" sz={sz} />,
  jl:  (sz) => <Badge bg="#9558B2" text="Jl" sz={sz} />,
  ex:  (sz) => <Badge bg="#6E4A7E" text="Ex" sz={sz} />,
  exs: (sz) => <Badge bg="#6E4A7E" text="Ex" sz={sz} />,
  erl: (sz) => <Badge bg="#A90533" text="Er" sz={sz} />,
  pl:  (sz) => <Badge bg="#39457E" text="Pl" sz={sz} />,

  // Apple
  swift: (sz) => <SwiftLogo sz={sz} />,
  m:     (sz) => <Badge bg="#438EFF" text="OC" sz={sz} />,

  // Shell
  sh:   (sz) => <ShellIcon sz={sz} />,
  bash: (sz) => <ShellIcon sz={sz} />,
  zsh:  (sz) => <ShellIcon sz={sz} />,
  fish: (sz) => <ShellIcon sz={sz} />,
  ps1:  (sz) => <Badge bg="#012456" text="PS" sz={sz} />,
  bat:  (sz) => <Badge bg="#4D4D4D" text=">" sz={sz} />,

  // Config / Data
  json:  (sz) => <JsonIcon sz={sz} />,
  jsonc: (sz) => <JsonIcon sz={sz} />,
  yaml:  (sz) => <Badge bg="#CB171E" text="Y" sz={sz} />,
  yml:   (sz) => <Badge bg="#CB171E" text="Y" sz={sz} />,
  toml:  (sz) => <Badge bg="#9C4121" text="T" sz={sz} />,
  xml:   (sz) => <Badge bg="#0060AC" text="<>" sz={sz} />,
  svg:   (sz) => <Badge bg="#FFB13B" fg="#1a1a1a" text="Sv" sz={sz} />,
  ini:   (sz) => <Badge bg="#6D8086" text="ini" sz={sz} />,
  env:   (sz) => <Badge bg="#ECD53F" fg="#1a1a1a" text=".E" sz={sz} />,

  // Docs
  md:  (sz) => <MarkdownLogo sz={sz} />,
  mdx: (sz) => <MarkdownLogo sz={sz} />,

  // Query
  sql:     (sz) => <Badge bg="#336791" text="SQ" sz={sz} />,
  graphql: (sz) => <Badge bg="#E10098" text="Gq" sz={sz} />,
  gql:     (sz) => <Badge bg="#E10098" text="Gq" sz={sz} />,
  prisma:  (sz) => <Badge bg="#2D3748" text="Pr" sz={sz} />,

  // DevOps / IaC
  tf:  (sz) => <Badge bg="#7B42BC" text="TF" sz={sz} />,
  hcl: (sz) => <Badge bg="#7B42BC" text="HC" sz={sz} />,

  // Functional
  hs:  (sz) => <Badge bg="#5e5086" text="λ" sz={sz} />,

  // Other
  dart: (sz) => <Badge bg="#0175C2" text="D" sz={sz} />,
  sol:  (sz) => <Badge bg="#363636" text="◆" sz={sz} />,
  diff: (sz) => <Badge bg="#41B883" text="±" sz={sz} />,
  lock: (sz) => <Badge bg="#4D4D4D" text="🔒" sz={sz} />,
  wasm: (sz) => <Badge bg="#654FF0" text="W" sz={sz} />,
}

const FILENAME_ICONS: Record<string, IconRenderer> = {
  dockerfile: (sz) => <DockerLogo sz={sz} />,
  'docker-compose.yml': (sz) => <DockerLogo sz={sz} />,
  'docker-compose.yaml': (sz) => <DockerLogo sz={sz} />,
  '.gitignore': (sz) => <GitLogo sz={sz} />,
  '.gitmodules': (sz) => <GitLogo sz={sz} />,
  '.gitattributes': (sz) => <GitLogo sz={sz} />,
  'package.json': (sz) => <Badge bg="#CB3837" text="npm" sz={sz} />,
  'package-lock.json': (sz) => <Badge bg="#CB3837" text="npm" sz={sz} />,
  'tsconfig.json': (sz) => <Badge bg="#3178C6" text="TS" sz={sz} />,
  'vite.config.ts': (sz) => <Badge bg="#646CFF" text="Vi" sz={sz} />,
  'vite.config.js': (sz) => <Badge bg="#646CFF" text="Vi" sz={sz} />,
  'cargo.toml': (sz) => <RustLogo sz={sz} />,
  'cargo.lock': (sz) => <RustLogo sz={sz} />,
  'go.mod': (sz) => <Badge bg="#00ADD8" text="Go" sz={sz} />,
  'go.sum': (sz) => <Badge bg="#00ADD8" text="Go" sz={sz} />,
  makefile: (sz) => <Badge bg="#6D8086" text="Mk" sz={sz} />,
  'cmakelists.txt': (sz) => <Badge bg="#064F8C" text="CM" sz={sz} />,
  license: (sz) => <Badge bg="#D4A868" text="©" sz={sz} />,
  'readme.md': (sz) => <MarkdownLogo sz={sz} />,
}

/* ── Fallback: colored dot with nothing else ──────────────────── */
function FallbackIcon({ sz, color }: { sz: number; color: string }) {
  return (
    <svg width={sz} height={sz} viewBox="0 0 16 16" fill="none">
      <rect x="2" y="1" width="10" height="14" rx="2" stroke={color} strokeWidth="1" fill="none"/>
      <path d="M8 1V4H12" stroke={color} strokeWidth="1" strokeLinejoin="round" fill="none"/>
    </svg>
  )
}

/* ═══════════════════════════════════════════════════════════════
   MAIN COMPONENT
═══════════════════════════════════════════════════════════════ */
export function LanguageIcon({ filename, size = 16, className }: LanguageIconProps) {
  const name = filename.split('/').pop()?.toLowerCase() || ''
  const ext = name.includes('.') ? name.split('.').pop() || '' : ''

  // 1. Try exact filename match
  const byName = FILENAME_ICONS[name]
  if (byName) return <span className={className} style={{ display: 'inline-flex', flexShrink: 0 }}>{byName(size)}</span>

  // 2. Try prefix match (e.g. "dockerfile.dev")
  for (const [key, renderer] of Object.entries(FILENAME_ICONS)) {
    if (name.startsWith(key)) return <span className={className} style={{ display: 'inline-flex', flexShrink: 0 }}>{renderer(size)}</span>
  }

  // 3. Try extension match
  if (ext && EXT_ICONS[ext]) {
    return <span className={className} style={{ display: 'inline-flex', flexShrink: 0 }}>{EXT_ICONS[ext](size)}</span>
  }

  // 4. Fallback to colored file icon from existing system
  const fallback = getFileIcon(filename)
  return <span className={className} style={{ display: 'inline-flex', flexShrink: 0 }}><FallbackIcon sz={size} color={fallback.color} /></span>
}
