package graph

// FileRecord represents a source file in the project.
type FileRecord struct {
	Path string // project-relative path (forward slashes)
	Hash string // xxHash hex digest of contents
}

// Symbol represents an extracted code symbol.
type Symbol struct {
	Name       string
	Kind       string // function, class, interface, method, struct, enum, const, type
	FilePath   string // which file this symbol lives in
	StartLine  int
	EndLine    int
	IsExported bool
	Signature  string // full signature text without body
}

// Edge represents a relationship between two files.
type Edge struct {
	SourcePath string // importing file
	TargetPath string // imported file
	Type       string // IMPORTS (CALLS, EXTENDS, IMPLEMENTS in M2)
}
