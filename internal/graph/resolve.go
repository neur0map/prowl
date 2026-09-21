// Package graph resolves the raw edges produced by extractors into a connected
// graph (include trees, exec/keybind chains, resource references) and infers
// per-file roles. Resolution is global and idempotent: it re-resolves the whole
// edge set each run, so incremental re-parsing of changed files stays correct.
package graph

import (
	"encoding/json"
	"path"
	"strings"

	"github.com/neur0map/prowl/internal/store"
)

// Resolve clears prior resolution and re-links every edge it can.
func Resolve(s *store.Store) error {
	if err := s.ResetResolution(); err != nil {
		return err
	}
	files, err := s.AllFiles()
	if err != nil {
		return err
	}
	fileMap := make(map[string]int64, len(files))
	byID := make(map[int64]store.File, len(files))
	rels := make([]string, 0, len(files))
	for _, f := range files {
		fileMap[f.RelPath] = f.ID
		byID[f.ID] = f
		rels = append(rels, f.RelPath)
	}
	byBase := make(map[string][]int64, len(files))
	for _, f := range files {
		byBase[path.Base(f.RelPath)] = append(byBase[path.Base(f.RelPath)], f.ID)
	}
	qmlStem := make(map[string][]store.File)
	for _, f := range files {
		if f.Lang == "qml" {
			b := path.Base(f.RelPath)
			stem := b[:len(b)-len(path.Ext(b))]
			qmlStem[stem] = append(qmlStem[stem], f)
		}
	}

	// Pass 1: include/import/require/source edges -> files.
	inc, err := s.UnresolvedEdges("includes", "references")
	if err != nil {
		return err
	}
	for _, e := range inc {
		if id, ok := resolvePath(fileMap, rels, e.File, e.Raw, byID[e.FileID].Lang); ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}

	// Pass 2: exec/autostart/keybind command strings -> script files.
	ex, err := s.UnresolvedEdges("execs", "binds", "autostarts")
	if err != nil {
		return err
	}
	for _, e := range ex {
		if id, ok := resolveCommandTarget(fileMap, rels, byBase, e.File, e.Raw); ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}

	// Pass 3: resource usages/declarations -> resource declarations.
	res, err := s.AllResources()
	if err != nil {
		return err
	}
	decls := make(map[string]int64)
	for _, r := range res {
		if r.Name != "" {
			if _, ok := decls[r.Name]; !ok {
				decls[r.Name] = r.ID
			}
		}
	}
	ruse, err := s.UnresolvedEdges("uses_resource", "declares_resource")
	if err != nil {
		return err
	}
	for _, e := range ruse {
		if id, ok := decls[e.Raw]; ok {
			if err := s.SetEdgeResolved(e.ID, "resource", id); err != nil {
				return err
			}
		}
	}
	// Pass 4: QML component instantiation -> the .qml file defining that component
	// (component name == file stem). Built-in/external types do not match and are
	// dropped afterward so they do not pollute the dangling set.
	inst, err := s.UnresolvedEdges("instantiates")
	if err != nil {
		return err
	}
	for _, e := range inst {
		if id, ok := resolveQMLComponent(qmlStem, byID[e.FileID].RelPath, e.Raw); ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}
	if err := s.DeleteUnresolvedEdges("instantiates"); err != nil {
		return err
	}
	// Pass 4b: type/member references by name -> the file declaring that type.
	// QML singletons/components (`Config.spacing`, `Button {}`) resolve by file
	// stem; Swift type usages (`StatCardView(...)`, `ServerStats`, `Theme.x`)
	// resolve to the struct/class/enum/protocol declaring that type -- Swift has
	// no file-level local imports, so type usage IS the file dependency.
	// Unresolved built-ins (Qt, Math, SwiftUI's View/Text/Int) are dropped
	// afterward so they never pollute the dangling set.
	swiftType, err := swiftTypeIndex(s, fileMap, byID)
	if err != nil {
		return err
	}
	uses, err := s.UnresolvedEdges("uses")
	if err != nil {
		return err
	}
	for _, e := range uses {
		var id int64
		var ok bool
		switch byID[e.FileID].Lang {
		case "qml":
			id, ok = resolveQMLComponent(qmlStem, byID[e.FileID].RelPath, e.Raw)
		case "swift":
			id, ok = firstOther(swiftType[e.Raw], e.FileID)
		}
		if ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}
	if err := s.DeleteUnresolvedEdges("uses"); err != nil {
		return err
	}
	// Pass 5: Go package imports -> the files of the imported in-module package.
	if err := resolveGoPackages(s, files, byID); err != nil {
		return err
	}
	// Pass 6: C# `using` imports -> the files declaring the imported namespace.
	if err := resolveNamespaceImports(s, byID, "csharp"); err != nil {
		return err
	}
	// Pass 7: Java/Kotlin imports -> the file for that class under any module's source root.
	if err := resolveJVMPackages(s, files, byID); err != nil {
		return err
	}
	// Pass 8: cross-crate Rust imports -> the module file in the target workspace crate.
	if err := resolveRustCrates(s, files, byID); err != nil {
		return err
	}
	// Pass 9: TS/JS workspace package imports -> the imported package's source entry.
	if err := resolveTSPackages(s, files, byID); err != nil {
		return err
	}
	// Pass 10: PHP `use Ns\Class` imports -> the file declaring that class.
	if err := resolvePHPNamespaces(s, byID); err != nil {
		return err
	}
	// Pass 11: Dart `package:<name>/x` imports -> that workspace package's lib/ source.
	if err := resolveDartPackages(s, files, byID); err != nil {
		return err
	}
	// Pass 12: TS/JS tsconfig path aliases (`@/x` -> src/x) -> the real source.
	if err := resolveTSAliases(s, files, byID); err != nil {
		return err
	}
	// Pass 13: Elixir alias/import/use -> the file declaring that module.
	if err := resolveNamespaceImports(s, byID, "elixir"); err != nil {
		return err
	}
	return nil
}

// resolveTSAliases links a TypeScript/JavaScript import that matches a tsconfig
// path alias (e.g. `@/components/Button` with `"paths": {"@/*": ["src/*"]}`) to
// the real source file. The alias prefix -> directory map is recorded at index
// time from each tsconfig.json/jsconfig.json, scoped to that config's directory,
// so a monorepo's per-package aliases resolve against the nearest config.
func resolveTSAliases(s *store.Store, files []store.File, byID map[int64]store.File) error {
	raw, _ := s.GetMeta("ts_aliases")
	if raw == "" {
		return nil
	}
	type cfg struct {
		Dir     string              `json:"dir"`
		Aliases map[string][]string `json:"aliases"`
	}
	var configs []cfg
	if err := json.Unmarshal([]byte(raw), &configs); err != nil || len(configs) == 0 {
		return nil
	}
	fileMap := make(map[string]int64, len(files))
	for _, f := range files {
		fileMap[f.RelPath] = f.ID
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	for _, e := range inc {
		switch byID[e.FileID].Lang {
		case "typescript", "tsx", "javascript":
		default:
			continue
		}
		imp := strings.Trim(strings.TrimSpace(e.Raw), `"'`)
		if imp == "" || strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "/") {
			continue // relative or absolute, handled elsewhere
		}
		// Pick the nearest config: the one whose dir is the longest ancestor of
		// the importing file.
		fromDir := path.Dir(byID[e.FileID].RelPath)
		var aliases map[string][]string
		bestDir := -1
		for _, cf := range configs {
			if (cf.Dir == "." || fromDir == cf.Dir || strings.HasPrefix(fromDir+"/", cf.Dir+"/")) && len(cf.Dir) > bestDir {
				aliases, bestDir = cf.Aliases, len(cf.Dir)
			}
		}
		if aliases == nil {
			continue
		}
		// Longest matching alias prefix.
		var prefix string
		for ap := range aliases {
			if strings.HasPrefix(imp, ap) && len(ap) > len(prefix) {
				prefix = ap
			}
		}
		if prefix == "" {
			continue
		}
		rest := imp[len(prefix):]
		for _, target := range aliases[prefix] {
			if id, ok := tsModuleFile(fileMap, path.Join(target, rest)); ok && id != e.FileID {
				if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// tsModuleFile resolves a TS/JS module path (no leading ./) to an indexed source
// file, trying source extensions and a directory index, mapping an ESM .js
// extension to its .ts source, and finally the literal path for a real asset.
func tsModuleFile(fileMap map[string]int64, base string) (int64, bool) {
	exts := []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
	stem := base
	for _, e := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"} {
		if strings.HasSuffix(base, e) {
			stem = strings.TrimSuffix(base, e)
			break
		}
	}
	var c []string
	for _, e := range exts {
		c = append(c, stem+e)
	}
	for _, e := range exts {
		c = append(c, path.Join(stem, "index"+e))
	}
	c = append(c, base) // a real asset import (./styles.css)
	for _, cand := range c {
		if id, ok := fileMap[cand]; ok {
			return id, true
		}
	}
	return 0, false
}

// resolveDartPackages links a Dart `package:<name>/<path>` import to the file
// under that workspace package's lib/ directory. The package name -> directory
// map is recorded at index time from each pubspec.yaml, so an import of a
// first-party package (an app's own `package:myapp/...`, or another member of a
// melos monorepo) resolves to lib/<path>. Relative and part imports resolve as
// paths in the include pass; SDK (`dart:...`) and third-party packages match no
// workspace pubspec and stay informational.
func resolveDartPackages(s *store.Store, files []store.File, byID map[int64]store.File) error {
	raw, _ := s.GetMeta("dart_packages")
	if raw == "" {
		return nil
	}
	var pkgs map[string]string
	if err := json.Unmarshal([]byte(raw), &pkgs); err != nil || len(pkgs) == 0 {
		return nil
	}
	fileMap := make(map[string]int64, len(files))
	for _, f := range files {
		fileMap[f.RelPath] = f.ID
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	for _, e := range inc {
		if byID[e.FileID].Lang != "dart" {
			continue
		}
		rest, ok := strings.CutPrefix(e.Raw, "package:")
		if !ok {
			continue // relative/part import (resolved as a path) or dart: SDK
		}
		i := strings.Index(rest, "/")
		if i < 0 {
			continue
		}
		dir, ok := pkgs[rest[:i]]
		if !ok {
			continue // third-party package, not a workspace member
		}
		if id, ok := fileMap[path.Join(dir, "lib", rest[i+1:])]; ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolvePHPNamespaces links a PHP `use Ns\Class` import to the file that
// declares that class. PHP `use` imports a fully-qualified class name, and PSR-4
// gives one class per file named after the class, so a file's declared namespace
// (recorded as a "namespace" resource at parse time, like C#) plus its basename
// identify it. require/include path imports carry no namespace separator and are
// resolved as paths by the include pass instead. Third-party (vendor) classes
// declare no indexed namespace and stay informational.
func resolvePHPNamespaces(s *store.Store, byID map[int64]store.File) error {
	ns, err := s.NamespaceFiles()
	if err != nil {
		return err
	}
	nsFiles := map[string][]store.File{} // namespace -> php files declaring it
	for name, ids := range ns {
		for _, id := range ids {
			if f, ok := byID[id]; ok && f.Lang == "php" {
				nsFiles[name] = append(nsFiles[name], f)
			}
		}
	}
	if len(nsFiles) == 0 {
		return nil
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	for _, e := range inc {
		if byID[e.FileID].Lang != "php" {
			continue
		}
		fqcn := strings.TrimPrefix(e.Raw, "\\")
		i := strings.LastIndex(fqcn, "\\")
		if i < 0 {
			continue // a require/include path or a root-namespace class, not resolvable here
		}
		cands := nsFiles[fqcn[:i]]
		class := fqcn[i+1:]
		for _, c := range cands {
			if strings.TrimSuffix(path.Base(c.RelPath), ".php") == class && c.ID != e.FileID {
				if err := s.SetEdgeResolved(e.ID, "file", c.ID); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// resolveRustCrates links `use other_crate::module::Item` across a Cargo
// workspace. The crate-name -> src-root map (from each Cargo.toml [package]
// name) is recorded at index time; the remaining path resolves to a module file
// within that crate's src, like crate:: does within the local crate. External
// crates match no workspace member and stay informational.
func resolveRustCrates(s *store.Store, files []store.File, byID map[int64]store.File) error {
	raw, _ := s.GetMeta("rust_crates")
	if raw == "" {
		return nil
	}
	var crates map[string]string
	if err := json.Unmarshal([]byte(raw), &crates); err != nil || len(crates) == 0 {
		return nil
	}
	fileMap := make(map[string]int64, len(files))
	for _, f := range files {
		fileMap[f.RelPath] = f.ID
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	for _, e := range inc {
		if byID[e.FileID].Lang != "rust" {
			continue
		}
		rest := e.Raw
		if i := strings.Index(rest, "::{"); i >= 0 {
			rest = rest[:i]
		}
		rest = strings.TrimSuffix(rest, "::*")
		segs := strings.Split(rest, "::")
		src, ok := crates[segs[0]]
		if !ok {
			continue // external crate, not a workspace member
		}
		mod := segs[1:]
		resolve := func(id int64, present bool) bool {
			if present && id != e.FileID {
				_ = s.SetEdgeResolved(e.ID, "file", id)
				return true
			}
			return false
		}
		done := false
		for n := len(mod); n >= 1 && !done; n-- {
			p := src + "/" + strings.Join(mod[:n], "/")
			id, ok := fileMap[p+".rs"]
			done = resolve(id, ok)
			if !done {
				id, ok = fileMap[p+"/mod.rs"]
				done = resolve(id, ok)
			}
		}
		if !done {
			// A type/fn re-exported at the crate root: link to the crate's entry
			// file, whose `pub use` edges carry the dependency on to the real module.
			id, ok := fileMap[src+"/lib.rs"]
			if !resolve(id, ok) {
				id, ok = fileMap[src+"/main.rs"]
				resolve(id, ok)
			}
		}
	}
	return nil
}

// resolveTSPackages links a bare TypeScript/JavaScript import of a workspace
// package (`@scope/pkg`, `pkg/subpath`) to that package's source. The package
// name -> directory map is recorded at index time from each package.json. The
// import resolves to a source file by the src/ convention (src/index for the
// package root, src/<subpath> or its directory index for a subpath), not by
// following package.json "exports", which point at built dist/ output that is
// not indexed. External packages match no workspace package.json and stay
// informational.
func resolveTSPackages(s *store.Store, files []store.File, byID map[int64]store.File) error {
	raw, _ := s.GetMeta("ts_packages")
	if raw == "" {
		return nil
	}
	var pkgs map[string]string
	if err := json.Unmarshal([]byte(raw), &pkgs); err != nil || len(pkgs) == 0 {
		return nil
	}
	fileMap := make(map[string]int64, len(files))
	for _, f := range files {
		fileMap[f.RelPath] = f.ID
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	for _, e := range inc {
		switch byID[e.FileID].Lang {
		case "typescript", "tsx", "javascript":
		default:
			continue
		}
		name, sub := tsImportPackage(e.Raw)
		dir, ok := pkgs[name]
		if !ok {
			continue // external package or relative import
		}
		if id, ok := tsPackageFile(fileMap, dir, sub); ok && id != e.FileID {
			if err := s.SetEdgeResolved(e.ID, "file", id); err != nil {
				return err
			}
		}
	}
	return nil
}

// tsImportPackage splits a bare module specifier into its package name and the
// subpath after it (no leading slash). A scoped specifier keeps its first two
// segments (@scope/pkg); an unscoped one keeps the first. Relative and absolute
// specifiers return an empty name.
func tsImportPackage(raw string) (name, sub string) {
	raw = strings.Trim(strings.TrimSpace(raw), `"'`)
	if raw == "" || strings.HasPrefix(raw, ".") || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~") {
		return "", ""
	}
	segs := strings.Split(raw, "/")
	n := 1
	if strings.HasPrefix(raw, "@") {
		n = 2
	}
	if len(segs) < n {
		return "", ""
	}
	return strings.Join(segs[:n], "/"), strings.Join(segs[n:], "/")
}

// tsPackageFile resolves a workspace package's directory and import subpath to
// an indexed source file. The package root (empty subpath) resolves to its
// index file; a subpath resolves to a matching source file, a directory index,
// or a real asset (a CSS/JSON import). A subpath that cites a .js/.mjs/.cjs
// extension (ESM/NodeNext) is matched against its .ts/.tsx source. Source
// extensions are tried .ts/.tsx first; src/ is preferred over the package root
// so built output is never hit.
func tsPackageFile(fileMap map[string]int64, dir, sub string) (int64, bool) {
	exts := []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
	var c []string
	if sub == "" {
		for _, base := range []string{path.Join(dir, "src", "index"), path.Join(dir, "index")} {
			for _, e := range exts {
				c = append(c, base+e)
			}
		}
	} else {
		// An ESM import cites a .js/.mjs/.cjs (or .ts) extension that points at
		// the .ts/.tsx source; strip it to a base and try every source
		// extension. The literal subpath is tried last for a real asset.
		stem := sub
		for _, e := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"} {
			if strings.HasSuffix(sub, e) {
				stem = strings.TrimSuffix(sub, e)
				break
			}
		}
		for _, b := range []string{path.Join(dir, "src", stem), path.Join(dir, stem)} {
			for _, e := range exts {
				c = append(c, b+e)
			}
			for _, e := range exts {
				c = append(c, path.Join(b, "index"+e))
			}
		}
		c = append(c, path.Join(dir, "src", sub), path.Join(dir, sub)) // real asset
	}
	for _, cand := range c {
		if id, ok := fileMap[cand]; ok {
			return id, true
		}
	}
	return 0, false
}

// resolveJVMPackages resolves Java and Kotlin imports across a multi-module
// project. A file's package-relative path (after src/main/java, src/main/kotlin,
// a src/test root, or src/) is its key, so `import com.foo.Bar` resolves to
// <module>/.../com/foo/Bar.java or Bar.kt in any module; Java and Kotlin resolve
// to each other in a mixed JVM project, and `import com.foo.*` fans out to every
// file in that package. Third-party and JDK imports match nothing and stay
// informational.
func resolveJVMPackages(s *store.Store, files []store.File, byID map[int64]store.File) error {
	pkgFiles := map[string]int64{} // com/foo/Bar.java or .kt -> id
	pkgDir := map[string][]int64{} // com/foo -> ids (for wildcard imports)
	for _, f := range files {
		if f.Lang != "java" && f.Lang != "kotlin" {
			continue
		}
		rel := javaPkgPath(f.RelPath)
		if rel == "" {
			continue
		}
		pkgFiles[rel] = f.ID
		pkgDir[path.Dir(rel)] = append(pkgDir[path.Dir(rel)], f.ID)
	}
	if len(pkgFiles) == 0 {
		return nil
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	var pkgEdges []store.PkgEdge
	for _, e := range inc {
		if l := byID[e.FileID].Lang; l != "java" && l != "kotlin" {
			continue
		}
		if pkg, ok := strings.CutSuffix(e.Raw, ".*"); ok {
			dir := strings.ReplaceAll(pkg, ".", "/")
			for _, dst := range pkgDir[dir] {
				if dst != e.FileID {
					pkgEdges = append(pkgEdges, store.PkgEdge{FileID: e.FileID, DstFileID: dst, Line: e.Line, Raw: e.Raw})
				}
			}
			continue
		}
		// Resolve to the file declaring the imported class, trying the full path
		// first and then progressively shorter prefixes, so a member or
		// nested-type import (com.foo.Outer.Inner, okhttp3.HttpUrl.Companion.toX)
		// links to its enclosing class file. The fallback only fires when the
		// exact path is not a file, so it cannot mislink a real class.
		for base := strings.ReplaceAll(e.Raw, ".", "/"); base != ""; {
			dst, ok := pkgFiles[base+".java"]
			if !ok {
				dst, ok = pkgFiles[base+".kt"]
			}
			if ok && dst != e.FileID {
				if err := s.SetEdgeResolved(e.ID, "file", dst); err != nil {
					return err
				}
				break
			}
			i := strings.LastIndex(base, "/")
			if i < 1 {
				break
			}
			base = base[:i]
		}
	}
	return s.AddPackageEdges(pkgEdges)
}

// javaPkgPath returns a JVM source file's path relative to its source root, i.e.
// its package directory plus filename. It locates a `src/` directory, then an
// optional `<sourceSet>/kotlin` or `<sourceSet>/java` segment (covering the
// Maven `src/main/java`, Gradle, and Kotlin-Multiplatform `src/jvmMain/kotlin`
// layouts); whatever follows is the package path. Files with no recognizable
// source root keep their full path.
func javaPkgPath(rel string) string {
	var after string
	switch {
	case strings.HasPrefix(rel, "src/"):
		after = rel[len("src/"):]
	case strings.Contains(rel, "/src/"):
		after = rel[strings.Index(rel, "/src/")+len("/src/"):]
	default:
		return rel
	}
	for _, m := range []string{"/kotlin/", "/java/"} {
		if i := strings.Index(after, m); i >= 0 {
			return after[i+len(m):]
		}
	}
	return after
}

// resolveNamespaceImports materializes namespace/module dependencies for a
// language that records each declared namespace (C# `namespace`, Elixir
// `defmodule`) as a namespace resource. An import of a name (`using Foo.Bar`,
// `alias Foo.Bar`) gets a synthetic resolved "pkg" edge to every same-language
// file declaring that namespace, so impact, callers, clusters, and entrypoints
// work across the project. Imports matching no declared namespace (framework or
// third-party) stay informational. Targets are filtered to the same language so
// a C# namespace and an Elixir module that share a dotted name never cross-link.
func resolveNamespaceImports(s *store.Store, byID map[int64]store.File, lang string) error {
	nsFiles, err := s.NamespaceFiles()
	if err != nil {
		return err
	}
	if len(nsFiles) == 0 {
		return nil
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	var pkgEdges []store.PkgEdge
	var resolvedImports []int64
	for _, e := range inc {
		if byID[e.FileID].Lang != lang {
			continue
		}
		resolvedToNamespace := false
		for _, dst := range nsFiles[e.Raw] {
			if byID[dst].Lang != lang {
				continue
			}
			resolvedToNamespace = true // the import names an in-repo namespace
			if dst == e.FileID {
				continue
			}
			pkgEdges = append(pkgEdges, store.PkgEdge{FileID: e.FileID, DstFileID: dst, Line: e.Line, Raw: e.Raw})
		}
		if resolvedToNamespace {
			// Mark the import resolved (non-"file" dst type, like Go packages) so it
			// is not miscounted as dangling and callees shows it as an internal
			// dependency, while the per-file "pkg" fan-out stays the caller source.
			resolvedImports = append(resolvedImports, e.ID)
		}
	}
	if err := s.AddPackageEdges(pkgEdges); err != nil {
		return err
	}
	for _, id := range resolvedImports {
		if err := s.SetEdgeResolved(id, "package", 0); err != nil {
			return err
		}
	}
	return nil
}

// resolveGoPackages materializes Go package dependencies. A Go file importing an
// in-module package gets a synthetic resolved "pkg" edge to every file of that
// package, so impact, callers, clusters, and entrypoints work across Go
// packages. Standard-library and external imports do not match the module
// prefix and are left as informational unresolved imports.
func resolveGoPackages(s *store.Store, files []store.File, byID map[int64]store.File) error {
	module, _ := s.GetMeta("go_module")
	if module == "" {
		return nil
	}
	// Index the importable .go files in each package directory. `_test.go` files
	// are never part of a package's imported surface, so external importers must
	// not get dependency edges into them (that would falsely give test files a
	// package's whole in-degree -- inflating impact, hotspots, and brief).
	goDirFiles := map[string][]int64{}
	for _, f := range files {
		if f.Lang == "go" && !strings.HasSuffix(f.RelPath, "_test.go") {
			goDirFiles[path.Dir(f.RelPath)] = append(goDirFiles[path.Dir(f.RelPath)], f.ID)
		}
	}
	inc, err := s.UnresolvedEdges("includes")
	if err != nil {
		return err
	}
	var pkgEdges []store.PkgEdge
	var resolvedImports []int64
	for _, e := range inc {
		if byID[e.FileID].Lang != "go" {
			continue
		}
		dir, ok := moduleImportDir(module, e.Raw)
		if !ok {
			continue // stdlib or external import
		}
		if len(goDirFiles[dir]) == 0 {
			continue // in-module path but no indexed files; leave unresolved
		}
		// The import resolved to an in-repo package. Mark the originating
		// includes edge resolved (so it is not miscounted as dangling and callees
		// shows it as an internal dependency) using a non-"file" dst type, so the
		// per-file "pkg" fan-out below stays the single source of file-level
		// caller/impact edges and callers are not double-counted.
		resolvedImports = append(resolvedImports, e.ID)
		for _, dst := range goDirFiles[dir] {
			if dst == e.FileID {
				continue
			}
			pkgEdges = append(pkgEdges, store.PkgEdge{FileID: e.FileID, DstFileID: dst, Line: e.Line, Raw: e.Raw})
		}
	}
	if err := s.AddPackageEdges(pkgEdges); err != nil {
		return err
	}
	for _, id := range resolvedImports {
		if err := s.SetEdgeResolved(id, "package", 0); err != nil {
			return err
		}
	}
	return nil
}

// moduleImportDir maps an in-module Go import path to its repo-relative package
// directory. Returns false for the module root and for out-of-module imports.
func moduleImportDir(module, importPath string) (string, bool) {
	if importPath == module {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(importPath, module+"/"); ok {
		return rest, true
	}
	return "", false
}

// resolveQMLComponent links a QML component instantiation (by type name) to the
// .qml file defining it: same directory first (QML's implicit same-dir import),
// then a repo-unique stem, then the candidate sharing the longest path prefix.
func resolveQMLComponent(qmlStem map[string][]store.File, fromRel, name string) (int64, bool) {
	cands := qmlStem[name]
	switch len(cands) {
	case 0:
		return 0, false
	case 1:
		return cands[0].ID, true
	}
	fromDir := path.Dir(fromRel)
	best, bestScore := cands[0].ID, -1
	for _, c := range cands {
		cdir := path.Dir(c.RelPath)
		score := sharedPrefixLen(cdir, fromDir)
		if cdir == fromDir {
			score = 1 << 30
		}
		if score > bestScore {
			best, bestScore = c.ID, score
		}
	}
	return best, true
}

func sharedPrefixLen(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

// swiftTypeIndex maps each Swift type name (struct/class/enum/protocol) to the
// file(s) declaring it, for resolving type-usage edges. Extensions are excluded
// so a use points at the type's definition, not everywhere it is extended.
func swiftTypeIndex(s *store.Store, fileMap map[string]int64, byID map[int64]store.File) (map[string][]int64, error) {
	idx := map[string][]int64{}
	hasSwift := false
	for _, f := range byID {
		if f.Lang == "swift" {
			hasSwift = true
			break
		}
	}
	if !hasSwift {
		return idx, nil
	}
	for _, kind := range []string{"struct", "class", "enum", "protocol"} {
		hits, err := s.SymbolsByKind(kind)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			fid, ok := fileMap[h.File]
			if !ok || byID[fid].Lang != "swift" {
				continue
			}
			idx[h.Name] = append(idx[h.Name], fid)
		}
	}
	return idx, nil
}

// firstOther returns the first id that is not self, if any.
func firstOther(ids []int64, self int64) (int64, bool) {
	for _, id := range ids {
		if id != self {
			return id, true
		}
	}
	return 0, false
}

// resolvePath resolves a raw include/reference target to a file id.
func resolvePath(fileMap map[string]int64, rels []string, fromRel, raw, lang string) (int64, bool) {
	raw = strings.Trim(strings.TrimSpace(raw), `"'`)
	if raw == "" {
		return 0, false
	}
	for _, c := range pathCandidates(fromRel, raw, lang) {
		if id, ok := fileMap[c]; ok {
			return id, true
		}
	}
	// Unique-suffix fallback (handles dotfiles repos mapped into ~/.config).
	tail := raw
	for _, p := range []string{"~/.config/", "~/", "./", "/"} {
		tail = strings.TrimPrefix(tail, p)
	}
	if tail == "" {
		return 0, false
	}
	var match int64
	cnt := 0
	for _, rel := range rels {
		if rel == tail || strings.HasSuffix(rel, "/"+tail) {
			match = fileMap[rel]
			cnt++
		}
	}
	if cnt == 1 {
		return match, true
	}
	return 0, false
}

func pathCandidates(fromRel, raw, lang string) []string {
	var c []string
	if lang == "lua" && !strings.Contains(raw, "/") {
		mod := strings.ReplaceAll(raw, ".", "/")
		dir := path.Dir(fromRel)
		c = append(c,
			mod+".lua", mod+"/init.lua",
			"lua/"+mod+".lua", "lua/"+mod+"/init.lua",
			path.Join(dir, mod+".lua"), path.Join(dir, mod, "init.lua"),
			path.Join(dir, "lua", mod+".lua"), path.Join(dir, "lua", mod, "init.lua"),
		)
	}
	// TypeScript/JavaScript relative imports. Modern ESM (NodeNext) writes the
	// import with a .js/.mjs extension that actually points at a .ts/.tsx source
	// (`./foo.js` -> foo.ts), so strip a trailing JS/TS extension to a base and try
	// every source extension plus a directory index. The raw path is also tried
	// as-is for real non-source assets (`./styles.css`, `./data.json`). Package
	// imports (no ./ or ../) are external and stay informational.
	if (lang == "typescript" || lang == "tsx" || lang == "javascript") &&
		(strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "../")) {
		joined := path.Clean(path.Join(path.Dir(fromRel), raw))
		c = append(c, joined)
		base := joined
		for _, e := range []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx"} {
			if strings.HasSuffix(joined, e) {
				base = strings.TrimSuffix(joined, e)
				break
			}
		}
		for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
			c = append(c, base+ext, path.Join(base, "index"+ext))
		}
	}
	// Rust crate-relative imports (`use crate::a::b::c;`) map module path segments
	// to files under the importing file's crate root, longest match first (a
	// submodule a/b/c.rs, else the module a/b.rs that defines item c). The crate
	// root is the enclosing `src/` dir, so this works for both a single crate at
	// the repo root and a Cargo workspace (crates/<name>/src/...). super::/self::
	// and cross-crate imports stay informational.
	if lang == "rust" {
		if rest, ok := strings.CutPrefix(raw, "crate::"); ok {
			if i := strings.Index(rest, "::{"); i >= 0 {
				rest = rest[:i]
			}
			segs := strings.Split(rest, "::")
			srcRoot := "src/"
			if i := strings.Index(fromRel, "/src/"); i >= 0 {
				srcRoot = fromRel[:i+len("/src/")]
			}
			for n := len(segs); n >= 1; n-- {
				p := srcRoot + strings.Join(segs[:n], "/")
				c = append(c, p+".rs", p+"/mod.rs")
			}
			c = append(c, srcRoot+"lib.rs", srcRoot+"main.rs")
		}
		// `mod foo;` includes a sibling module file. In foo/mod.rs, lib.rs or
		// main.rs the submodule is a sibling (dir/foo.rs); in foo.rs it lives in a
		// subdir named after the file (dir/foo/bar.rs). Try both.
		if rest, ok := strings.CutPrefix(raw, "mod::"); ok {
			dir := path.Dir(fromRel)
			c = append(c, path.Join(dir, rest)+".rs", path.Join(dir, rest, "mod.rs"))
			base := strings.TrimSuffix(path.Base(fromRel), ".rs")
			if base != "mod" && base != "lib" && base != "main" {
				c = append(c, path.Join(dir, base, rest)+".rs", path.Join(dir, base, rest, "mod.rs"))
			}
		}
	}
	// Python absolute imports (`import a.b`, `from a.b import c`) map the dotted
	// module to a file: a/b.py or a/b/__init__.py, also under src/ for that
	// layout. Third-party imports stay informational.
	if lang == "python" && raw != "" && !strings.HasPrefix(raw, ".") {
		mod := strings.ReplaceAll(raw, ".", "/")
		c = append(c, mod+".py", mod+"/__init__.py", "src/"+mod+".py", "src/"+mod+"/__init__.py")
	}
	// Python relative imports (`from .foo import x`, `from ..pkg.mod import y`):
	// the leading dots count levels up from the importing file's package, and the
	// remaining dotted path maps to a file there. A bare `from . import x` has no
	// module path and stays informational.
	if lang == "python" && strings.HasPrefix(raw, ".") {
		dots := 0
		for dots < len(raw) && raw[dots] == '.' {
			dots++
		}
		if modpath := strings.ReplaceAll(raw[dots:], ".", "/"); modpath != "" {
			base := path.Dir(fromRel)
			for i := 1; i < dots; i++ {
				base = path.Dir(base)
			}
			target := path.Join(base, modpath)
			c = append(c, target+".py", target+"/__init__.py")
		}
	}
	// Java imports (`import com.foo.Bar;`) map the dotted class path to a file,
	// including the Maven/Gradle src/main/java layout. Wildcard and third-party
	// imports do not match a project file and stay informational.
	if lang == "java" && raw != "" {
		mod := strings.ReplaceAll(raw, ".", "/")
		c = append(c, mod+".java", "src/main/java/"+mod+".java", "src/"+mod+".java")
	}
	// Ruby require/require_relative: require_relative is relative to the requiring
	// file's directory; require resolves from the project root or lib/. Gems do
	// not match a project file and stay informational.
	if lang == "ruby" && raw != "" {
		c = append(c, raw+".rb", "lib/"+raw+".rb",
			path.Clean(path.Join(path.Dir(fromRel), raw))+".rb")
	}
	if strings.HasPrefix(raw, "~/.config/") {
		c = append(c, strings.TrimPrefix(raw, "~/.config/"))
	}
	if strings.HasPrefix(raw, "~/") {
		c = append(c, strings.TrimPrefix(raw, "~/"))
	}
	if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "~") {
		c = append(c, path.Clean(path.Join(path.Dir(fromRel), raw)))
	}
	c = append(c, strings.TrimPrefix(raw, "/"))
	return c
}

// resolveCommandTarget resolves a command string to an indexed file: first the
// bare command name against repo command files by basename (e.g. ryoku-pkg-add
// -> bin/ryoku-pkg-add), then any path-like token referring to a script.
func resolveCommandTarget(fileMap map[string]int64, rels []string, byBase map[string][]int64, fromRel, cmd string) (int64, bool) {
	toks := strings.Fields(cmd)
	if len(toks) > 0 {
		first := strings.Trim(toks[0], `"'`)
		if first != "" && !strings.ContainsAny(first, "/$") {
			if ids, ok := byBase[first]; ok && len(ids) == 1 {
				return ids[0], true
			}
		}
	}
	for _, t := range toks {
		t = strings.Trim(t, `"'`)
		if strings.Contains(t, "/") || strings.HasSuffix(t, ".sh") || strings.HasSuffix(t, ".py") || strings.HasSuffix(t, ".lua") {
			if id, ok := resolvePath(fileMap, rels, fromRel, t, ""); ok {
				return id, true
			}
		}
	}
	return 0, false
}
