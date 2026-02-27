type FileIconDef = { color: string; label: string };

// ── Category color bands ──────────────────────────
// Docs:   steel blue    #7EAAC4
// Config: muted salmon  #C4907A
// Code:   per-language identity, slightly muted
// Shell:  olive green   #8AAA78
// Query:  warm amber    #CCA060
// Images: neutral gray  #7A7F84

const EXT_MAP: Record<string, FileIconDef> = {
  // JavaScript / TypeScript — muted brand colors
  js:   { color: '#D6C960', label: 'JS' },
  mjs:  { color: '#D6C960', label: 'JS' },
  cjs:  { color: '#D6C960', label: 'JS' },
  jsx:  { color: '#6CC4C4', label: 'JSX' },
  ts:   { color: '#6AAAD4', label: 'TS' },
  mts:  { color: '#6AAAD4', label: 'TS' },
  cts:  { color: '#6AAAD4', label: 'TS' },
  tsx:  { color: '#6AAAD4', label: 'TSX' },

  // Web — code category
  html: { color: '#D08060', label: 'HTML' },
  htm:  { color: '#D08060', label: 'HTML' },
  css:  { color: '#60A8D0', label: 'CSS' },
  scss: { color: '#C080A0', label: 'SCSS' },
  sass: { color: '#C080A0', label: 'SASS' },
  less: { color: '#80A0C0', label: 'LESS' },
  vue:  { color: '#6AB88A', label: 'Vue' },
  svelte: { color: '#D86040', label: 'Sv' },

  // Systems — code category
  rs:   { color: '#D0884C', label: 'Rs' },
  go:   { color: '#60B8B8', label: 'Go' },
  c:    { color: '#90A8BC', label: 'C' },
  h:    { color: '#90A8BC', label: 'H' },
  cpp:  { color: '#6898B8', label: 'C++' },
  cc:   { color: '#6898B8', label: 'C++' },
  hpp:  { color: '#6898B8', label: 'H++' },
  zig:  { color: '#D0A040', label: 'Zig' },

  // JVM — code category
  java: { color: '#CC9050', label: 'Jv' },
  kt:   { color: '#A08CE0', label: 'Kt' },
  kts:  { color: '#A08CE0', label: 'Kt' },
  scala: { color: '#C06060', label: 'Sc' },
  groovy: { color: '#60A0B0', label: 'Gr' },
  clj:  { color: '#80A0D0', label: 'Clj' },

  // .NET — code category
  cs:   { color: '#9880D0', label: 'C#' },
  fs:   { color: '#60A8C8', label: 'F#' },
  vb:   { color: '#9878B0', label: 'VB' },

  // Scripting — code category
  py:   { color: '#70A0CC', label: 'Py' },
  pyi:  { color: '#70A0CC', label: 'Py' },
  rb:   { color: '#C06058', label: 'Rb' },
  php:  { color: '#8890B0', label: 'PHP' },
  lua:  { color: '#6878B8', label: 'Lua' },
  r:    { color: '#6898C0', label: 'R' },
  jl:   { color: '#9870A8', label: 'Jl' },
  ex:   { color: '#9878B0', label: 'Ex' },
  exs:  { color: '#9878B0', label: 'Ex' },
  erl:  { color: '#B06888', label: 'Erl' },
  pl:   { color: '#7080B0', label: 'Pl' },

  // Apple — code category
  swift: { color: '#D07050', label: 'Sw' },
  m:    { color: '#5890D0', label: 'OC' },

  // Shell — olive green band
  sh:   { color: '#8AAA78', label: 'Sh' },
  bash: { color: '#8AAA78', label: 'Sh' },
  zsh:  { color: '#8AAA78', label: 'Sh' },
  fish: { color: '#8AAA78', label: 'Sh' },
  ps1:  { color: '#6888B0', label: 'PS' },
  bat:  { color: '#A0B870', label: 'Bat' },

  // Config — muted salmon band
  json: { color: '#C4907A', label: '{ }' },
  jsonc: { color: '#C4907A', label: '{ }' },
  yaml: { color: '#C4907A', label: 'Yml' },
  yml:  { color: '#C4907A', label: 'Yml' },
  toml: { color: '#C4907A', label: 'Toml' },
  xml:  { color: '#C4907A', label: 'XML' },
  svg:  { color: '#C4907A', label: 'SVG' },
  ini:  { color: '#C4907A', label: 'Ini' },
  env:  { color: '#C4907A', label: 'Env' },

  // Docs — steel blue band
  md:   { color: '#7EAAC4', label: 'MD' },
  mdx:  { color: '#7EAAC4', label: 'MDX' },
  txt:  { color: '#8A9AA8', label: 'Txt' },

  // Query — warm amber band
  sql:  { color: '#CCA060', label: 'SQL' },
  graphql: { color: '#C080A0', label: 'GQL' },
  gql:  { color: '#C080A0', label: 'GQL' },
  prisma: { color: '#8898A8', label: 'Prm' },

  // DevOps — code category
  tf:   { color: '#9880C0', label: 'TF' },
  hcl:  { color: '#9880C0', label: 'HCL' },

  // Functional — code category
  hs:   { color: '#8880A8', label: 'Hs' },

  // Other
  dart: { color: '#60A8CC', label: 'Dart' },
  sol:  { color: '#888888', label: 'Sol' },
  proto: { color: '#7A7F84', label: 'PB' },
  diff: { color: '#70B090', label: 'Diff' },
  lock: { color: '#7A7F84', label: 'Lock' },
  wasm: { color: '#9080D0', label: 'Wasm' },

  // Images — neutral gray
  png:  { color: '#7A7F84', label: 'Img' },
  jpg:  { color: '#7A7F84', label: 'Img' },
  jpeg: { color: '#7A7F84', label: 'Img' },
  gif:  { color: '#7A7F84', label: 'Img' },
  ico:  { color: '#7A7F84', label: 'Ico' },
};

const FILENAME_MAP: Record<string, FileIconDef> = {
  'dockerfile':     { color: '#60A8CC', label: 'Dock' },
  'makefile':       { color: '#8AAA78', label: 'Make' },
  'cmakelists.txt': { color: '#6898B8', label: 'CMk' },
  'cargo.toml':     { color: '#D0884C', label: 'Crgo' },
  'cargo.lock':     { color: '#D0884C', label: 'Crgo' },
  'go.mod':         { color: '#60B8B8', label: 'GoM' },
  'go.sum':         { color: '#60B8B8', label: 'GoS' },
  'package.json':   { color: '#C4907A', label: 'npm' },
  'tsconfig.json':  { color: '#C4907A', label: 'TSc' },
  'vite.config.ts': { color: '#9080D0', label: 'Vite' },
  '.gitignore':     { color: '#D07050', label: 'Git' },
  '.env':           { color: '#C4907A', label: 'Env' },
  'license':        { color: '#CCA060', label: 'Lic' },
  'readme.md':      { color: '#7EAAC4', label: 'Rdm' },
};

const DEFAULT_ICON: FileIconDef = { color: '#7A7F84', label: 'File' };

export function getFileIcon(filePath: string): FileIconDef {
  const name = filePath.split('/').pop()?.toLowerCase() || '';

  const byName = FILENAME_MAP[name];
  if (byName) return byName;

  for (const [key, val] of Object.entries(FILENAME_MAP)) {
    if (name.startsWith(key)) return val;
  }

  const ext = name.includes('.') ? name.split('.').pop() || '' : '';
  if (ext && EXT_MAP[ext]) return EXT_MAP[ext];

  if (name.endsWith('.d.ts')) return EXT_MAP['ts'];

  return DEFAULT_ICON;
}
