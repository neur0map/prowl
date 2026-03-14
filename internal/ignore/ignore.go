package ignore

import (
	"path/filepath"
	"strings"

	gitignore "github.com/sabhiram/go-gitignore"
)

// Segments that cause an entire directory subtree to be skipped.
var ignoredSegments = map[string]bool{
	// SCM
	".git": true, ".svn": true, ".hg": true, ".bzr": true,
	// Prowl
	".prowl": true,
	// Dependencies
	"node_modules": true, "bower_components": true, "jspm_packages": true,
	"vendor": true, "venv": true, ".venv": true, "env": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	"site-packages": true, ".tox": true, "eggs": true, ".eggs": true,
	"Pods": true, "Carthage": true, ".swiftpm": true, "xcuserdata": true,
	// Build output
	"dist": true, "build": true, "out": true, "output": true,
	"bin": true, "obj": true, "target": true,
	".next": true, ".nuxt": true, ".output": true,
	".vercel": true, ".netlify": true, ".serverless": true,
	"_build": true, ".parcel-cache": true, ".turbo": true, ".svelte-kit": true,
	".generated": true, "generated": true,
	".terraform": true,
	"coverage": true, ".nyc_output": true, "htmlcov": true, ".coverage": true,
	"DerivedData": true, ".build": true, "Build": true, "Products": true,
	"release": true, "releases": true,
	"logs": true, "log": true, "tmp": true, "temp": true,
	"cache": true, ".cache": true, ".tmp": true, ".temp": true,
	// Editor / tooling
	".idea": true, ".vscode": true, ".vs": true, ".eclipse": true, ".settings": true,
	".husky": true, ".github": true, ".circleci": true, ".gitlab": true,
}

// File extensions that are always ignored.
var ignoredExtensions = map[string]bool{
	// Binaries
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".a": true,
	".lib": true, ".o": true, ".obj": true,
	".class": true, ".jar": true, ".war": true, ".ear": true,
	".pyc": true, ".pyo": true, ".pyd": true,
	".beam": true, ".wasm": true, ".node": true,
	".map": true,
	".bin": true, ".dat": true, ".data": true, ".raw": true,
	".iso": true, ".img": true, ".dmg": true,
	// Media
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".webp": true, ".bmp": true, ".tiff": true, ".tif": true,
	".psd": true, ".ai": true, ".sketch": true, ".fig": true, ".xd": true,
	// Archives
	".zip": true, ".tar": true, ".gz": true, ".rar": true, ".7z": true,
	".bz2": true, ".xz": true, ".tgz": true,
	// Documents
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".ppt": true, ".pptx": true, ".odt": true, ".ods": true, ".odp": true,
	// Audio/video
	".mp4": true, ".mp3": true, ".wav": true, ".mov": true, ".avi": true,
	".mkv": true, ".flv": true, ".wmv": true, ".ogg": true, ".webm": true,
	".flac": true, ".aac": true, ".m4a": true,
	// Fonts
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true, ".otf": true,
	// Data files
	".db": true, ".sqlite": true, ".sqlite3": true, ".mdb": true, ".accdb": true,
	".csv": true, ".tsv": true, ".parquet": true, ".avro": true, ".feather": true,
	".npy": true, ".npz": true, ".pkl": true, ".pickle": true, ".h5": true, ".hdf5": true,
	// Credentials
	".pem": true, ".key": true, ".crt": true, ".cer": true, ".p12": true, ".pfx": true,
	// Lock files
	".lock": true,
}

// Exact filenames that are always ignored.
var ignoredFilenames = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"composer.lock": true, "Gemfile.lock": true, "poetry.lock": true,
	"Cargo.lock": true, "go.sum": true,
	".gitignore": true, ".gitattributes": true, ".npmrc": true, ".yarnrc": true,
	".editorconfig": true, ".prettierrc": true, ".prettierignore": true,
	".eslintignore": true, ".dockerignore": true,
	".env": true, ".env.local": true, ".env.development": true,
	".env.production": true, ".env.test": true, ".env.example": true,
	"LICENSE": true, "LICENSE.md": true, "LICENSE.txt": true,
	"CHANGELOG.md": true, "CHANGELOG": true,
	"CONTRIBUTING.md": true, "CODE_OF_CONDUCT.md": true, "SECURITY.md": true,
	".DS_Store": true, "Thumbs.db": true,
}

// Checker holds compiled ignore rules.
type Checker struct {
	gi *gitignore.GitIgnore
}

// New creates a Checker. If projectRoot is non-empty, it reads
// projectRoot/.gitignore for additional patterns.
func New(projectRoot string) *Checker {
	c := &Checker{}
	if projectRoot != "" {
		giPath := filepath.Join(projectRoot, ".gitignore")
		gi, err := gitignore.CompileIgnoreFile(giPath)
		if err == nil {
			c.gi = gi
		}
	}
	return c
}

// ShouldIgnore returns true if the given relative path should be skipped.
func (c *Checker) ShouldIgnore(relPath string) bool {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	filename := parts[len(parts)-1]

	// Check path segments
	for _, seg := range parts {
		if ignoredSegments[seg] {
			return true
		}
	}

	// Check exact filename
	if ignoredFilenames[filename] {
		return true
	}

	// Check extension (handles compound like .min.js)
	lower := strings.ToLower(filename)
	ext := filepath.Ext(lower)
	if ext != "" && ignoredExtensions[ext] {
		return true
	}

	// Check compound extensions (.min.js, .d.ts, etc.)
	if strings.HasSuffix(lower, ".min.js") ||
		strings.HasSuffix(lower, ".min.css") ||
		strings.HasSuffix(lower, ".bundle.js") ||
		strings.HasSuffix(lower, ".chunk.js") ||
		strings.HasSuffix(lower, ".generated.ts") ||
		strings.HasSuffix(lower, ".generated.js") ||
		strings.HasSuffix(lower, ".d.ts") {
		return true
	}

	// Check .gitignore patterns
	if c.gi != nil && c.gi.MatchesPath(relPath) {
		return true
	}

	return false
}
