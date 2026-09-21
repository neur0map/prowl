package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/sketch"
)

// newSketchCmd renders a compact visual sketch of a UI component: its element
// tree, the visual properties on each element, and its behavior (handlers,
// animations, conditional visibility). It answers "how does this UI look and
// behave" without a screenshot, a runtime, or reading the whole file -- prowl's
// code-intelligence promise applied to declarative UI. The argument is a file
// path or a component name resolved through the index.
func newSketchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sketch <file-or-component>",
		Short: "Sketch how a UI looks and behaves without a screenshot: QML, React (jsx/tsx), Go/lipgloss, or CSS",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, a []string) error {
			format, err := resolveFormat(cmd, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			path, src, root, err := resolveSketchSource(cmd, a[0])
			if err != nil {
				return err
			}
			sk, err := sketch.Of(path, src)
			if err != nil {
				return err
			}
			// Resolve QML token references (Tokens.ink -> #cdd6f4) against the
			// singletons that ship in the project.
			if qs, ok := sk.(*sketch.Sketch); ok && root != "" {
				qs.Resolve(sketch.DirSingletonSource(root, path))
			}
			if format == formatJSON {
				str, err := formatValue(sk, formatJSON)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), sk.Text())
			return err
		},
	}
	return c
}

// resolveSketchSource turns the argument into a file path, its bytes, and the
// project root to resolve token singletons against. A path that exists on disk
// is read directly (no index needed); otherwise the argument is a component name
// resolved through the workspace index.
func resolveSketchSource(cmd *cobra.Command, arg string) (path string, src []byte, root string, err error) {
	if fi, statErr := os.Stat(arg); statErr == nil && !fi.IsDir() {
		src, err = os.ReadFile(arg)
		if err != nil {
			return "", nil, "", err
		}
		abs, absErr := filepath.Abs(arg)
		if absErr != nil {
			abs = arg
		}
		return abs, src, projectRoot(filepath.Dir(abs)), nil
	}
	// An argument that names a path (has a separator or a file extension) but
	// does not exist is a mistyped file, not a component name -- say so plainly
	// instead of reporting a confusing "no symbol named <path>".
	if strings.ContainsRune(arg, filepath.Separator) || strings.Contains(arg, ".") {
		return "", nil, "", fmt.Errorf("file not found: %s", arg)
	}
	q, ws, _, closer, err := openQuerier(cmd.Context(), false)
	if err != nil {
		return "", nil, "", err
	}
	defer closer()
	def, err := q.Definition(ws.Root, arg)
	if err != nil {
		return "", nil, "", err
	}
	path = filepath.Join(ws.Root, filepath.FromSlash(def.File))
	src, err = os.ReadFile(path)
	if err != nil {
		return "", nil, "", fmt.Errorf("read %s: %w", def.File, err)
	}
	return path, src, ws.Root, nil
}

// projectRoot walks up from dir to the nearest directory holding a project
// marker, falling back to dir itself, so token resolution scans the whole
// project rather than one leaf folder.
func projectRoot(dir string) string {
	for d := dir; ; {
		for _, marker := range []string{".prowl", ".git", "go.mod", "qmldir"} {
			if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
				return d
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}
