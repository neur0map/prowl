/* ── Filter rule shape ───────────────────────────────── */

interface Rule {
  type: 'segment' | 'extension' | 'filename' | 'pattern';
  value: string | RegExp;
}

/* ── Compiled output and build artifacts ────────────── */

const compiledOutput = {
  segments: [
    'dist', 'build', 'out', 'output', 'bin', 'obj', 'target',
    '.next', '.nuxt', '.output', '.vercel', '.netlify', '.serverless',
    '_build', 'public/build', '.parcel-cache', '.turbo', '.svelte-kit',
    '.generated', 'generated', 'auto-generated', '.terraform',
    'coverage', '.nyc_output', 'htmlcov', '.coverage',
    '__tests__', '__mocks__', '.jest',
    'logs', 'log', 'tmp', 'temp', 'cache', '.cache', '.tmp', '.temp',
  ],
  extensions: [
    '.exe', '.dll', '.so', '.dylib', '.a', '.lib', '.o', '.obj',
    '.class', '.jar', '.war', '.ear',
    '.pyc', '.pyo', '.pyd',
    '.beam', '.wasm', '.node',
    '.map',
    '.bin', '.dat', '.data', '.raw',
    '.iso', '.img', '.dmg',
  ],
};

/* ── Package manager and dependency directories ─────── */

const packageDeps = {
  segments: [
    'node_modules', 'bower_components', 'jspm_packages', 'vendor',
    'venv', '.venv', 'env', '.env',
    '__pycache__', '.pytest_cache', '.mypy_cache', 'site-packages',
    '.tox', 'eggs', '.eggs', 'lib64', 'parts', 'sdist', 'wheels',
  ],
  extensions: [
    '.lock',
  ],
  filenames: [
    'package-lock.json', 'yarn.lock', 'pnpm-lock.yaml',
    'composer.lock', 'Gemfile.lock', 'poetry.lock', 'Cargo.lock', 'go.sum',
  ],
};

/* ── Editor and tooling configuration ───────────────── */

const editorSettings = {
  segments: [
    '.idea', '.vscode', '.vs', '.eclipse', '.settings',
    '.husky', '.github', '.circleci', '.gitlab',
    'fixtures', 'snapshots', '__snapshots__',
  ],
  extensions: [] as string[],
  filenames: [
    '.gitignore', '.gitattributes', '.npmrc', '.yarnrc',
    '.editorconfig', '.prettierrc', '.prettierignore',
    '.eslintignore', '.dockerignore',
  ],
};

/* ── Media, fonts, and binary assets ────────────────── */

const mediaAndBinary = {
  segments: [] as string[],
  extensions: [
    '.png', '.jpg', '.jpeg', '.gif', '.svg', '.ico', '.webp', '.bmp', '.tiff', '.tif',
    '.psd', '.ai', '.sketch', '.fig', '.xd',
    '.zip', '.tar', '.gz', '.rar', '.7z', '.bz2', '.xz', '.tgz',
    '.pdf', '.doc', '.docx', '.xls', '.xlsx', '.ppt', '.pptx',
    '.odt', '.ods', '.odp',
    '.mp4', '.mp3', '.wav', '.mov', '.avi', '.mkv', '.flv', '.wmv',
    '.ogg', '.webm', '.flac', '.aac', '.m4a',
    '.woff', '.woff2', '.ttf', '.eot', '.otf',
    '.db', '.sqlite', '.sqlite3', '.mdb', '.accdb',
    '.csv', '.tsv', '.parquet', '.avro', '.feather',
    '.npy', '.npz', '.pkl', '.pickle', '.h5', '.hdf5',
  ],
  filenames: [
    'Thumbs.db', '.DS_Store',
  ],
};

/* ── Sensitive or credential files ──────────────────── */

const sensitiveFiles = {
  segments: [] as string[],
  extensions: [
    '.pem', '.key', '.crt', '.cer', '.p12', '.pfx',
  ],
  filenames: [
    '.env', '.env.local', '.env.development', '.env.production',
    '.env.test', '.env.example',
    'SECURITY.md',
  ],
};

/* ── Source control metadata ────────────────────────── */

const scmMetadata = {
  segments: [
    '.git', '.svn', '.hg', '.bzr',
  ],
  extensions: [] as string[],
  filenames: [
    'LICENSE', 'LICENSE.md', 'LICENSE.txt',
    'CHANGELOG.md', 'CHANGELOG',
    'CONTRIBUTING.md', 'CODE_OF_CONDUCT.md',
  ],
};

/* ── Compound / generated file patterns ─────────────── */

const MINIFIED_PATTERN = /\.(?:min\.js|min\.css|bundle\.js|chunk\.js)$/;
const GENERATED_PATTERN = /\.(?:generated\.|d\.ts$)/;
const BUNDLED_PATTERN = /\.(?:bundle\.|chunk\.|generated\.)/;

/* ── Assemble all rules from every category ─────────── */

const allCategories = [compiledOutput, packageDeps, editorSettings, mediaAndBinary, sensitiveFiles, scmMetadata];

const rules: Rule[] = [];

for (const cat of allCategories) {
  for (const seg of cat.segments) {
    rules.push({ type: 'segment', value: seg });
  }
  for (const ext of cat.extensions) {
    rules.push({ type: 'extension', value: ext });
  }
  if ('filenames' in cat) {
    for (const fname of (cat as { filenames: string[] }).filenames) {
      rules.push({ type: 'filename', value: fname });
    }
  }
}

rules.push({ type: 'pattern', value: MINIFIED_PATTERN });
rules.push({ type: 'pattern', value: GENERATED_PATTERN });
rules.push({ type: 'pattern', value: BUNDLED_PATTERN });

/* ── Public predicate ───────────────────────────────── */

export function shouldIgnorePath(filePath: string): boolean {
  const posixPath = filePath.replace(/\\/g, '/');
  const parts = posixPath.split('/');
  const fileName = parts[parts.length - 1];
  const lowered = fileName.toLowerCase();

  const dotPos = lowered.lastIndexOf('.');
  let ext = '';
  let compoundExt = '';
  if (dotPos !== -1) {
    ext = lowered.substring(dotPos);
    const prevDot = lowered.lastIndexOf('.', dotPos - 1);
    if (prevDot !== -1) {
      compoundExt = lowered.substring(prevDot);
    }
  }

  for (const rule of rules) {
    switch (rule.type) {
      case 'segment':
        for (const part of parts) {
          if (part === rule.value) return true;
        }
        break;
      case 'extension':
        if (ext === rule.value) return true;
        if (compoundExt === rule.value) return true;
        break;
      case 'filename':
        if (fileName === rule.value || lowered === (rule.value as string).toLowerCase()) return true;
        break;
      case 'pattern':
        if ((rule.value as RegExp).test(lowered)) return true;
        break;
    }
  }

  return false;
}
