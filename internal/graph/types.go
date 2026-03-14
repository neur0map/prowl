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
	SourcePath string  // importing file
	TargetPath string  // imported file
	Type       string  // IMPORTS, CALLS, EXTENDS, IMPLEMENTS
	Confidence float64 // 0-1, used for CALLS/EXTENDS/IMPLEMENTS edges
}

// CallRef represents a function/method call site found in source code.
type CallRef struct {
	CalleeName string // the called function/method name
	Line       int    // line number of the call
}

// HeritageRef represents an extends or implements relationship.
type HeritageRef struct {
	ChildName  string // the class/type being defined
	ParentName string // the extended class or implemented interface
	Type       string // "extends" or "implements"
}
