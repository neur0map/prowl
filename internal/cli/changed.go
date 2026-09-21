package cli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/query"
)

// ChangedResult maps a working set of git changes to the indexed files each one
// could affect, so an agent can answer "I touched these files, what should I
// check?" before editing further or committing.
type ChangedResult struct {
	Base  string        `json:"base"`
	Files []ChangedFile `json:"files"`
}

// ChangedFile is one changed path with a token-lean summary of its downstream
// impact (dependent count, subsystem breakdown, direct importers) rather than
// the full dependent list, which floods on central files.
type ChangedFile struct {
	File    string         `json:"file"`
	Indexed bool           `json:"indexed"`
	Impact  *ChangedImpact `json:"impact,omitempty"`
}

// ChangedImpact is the blast-radius summary for one changed file.
type ChangedImpact struct {
	Total       int                    `json:"total"`
	Direct      int                    `json:"direct"`
	BySubsystem []query.SubsystemCount `json:"by_subsystem,omitempty"`
	DirectFiles []string               `json:"direct_files,omitempty"`
}

// changedFiles returns the project files that differ from base (default HEAD),
// as slash paths relative to the git root. With base HEAD it also includes
// untracked, non-ignored files, so newly added files are covered.
func changedFiles(root, base string) ([]string, error) {
	set := map[string]bool{}
	collect := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		return cmd.Output()
	}
	out, err := collect("diff", "--name-only", base)
	if err != nil {
		return nil, fmt.Errorf("git diff against %q failed (is this a git repo?): %w", base, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			set[filepath.ToSlash(line)] = true
		}
	}
	if base == "HEAD" {
		if un, err := collect("ls-files", "--others", "--exclude-standard"); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(un)), "\n") {
				if line != "" {
					set[filepath.ToSlash(line)] = true
				}
			}
		}
	}
	files := make([]string, 0, len(set))
	for f := range set {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

func newChangedCmd() *cobra.Command {
	var base string
	var all bool
	c := &cobra.Command{
		Use:   "changed",
		Short: "Map your git changes to the files they could affect (blast radius)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := resolveFormat(cmd, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			q, ws, s, closer, err := openQuerier(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closer()

			files, err := changedFiles(ws.Root, base)
			if err != nil {
				return err
			}
			sizes, _ := s.FileSizes()
			res := ChangedResult{Base: base, Files: make([]ChangedFile, 0, len(files))}
			for _, f := range files {
				_, indexed := sizes[f]
				// By default only indexed files are reported: those are the ones
				// with meaningful impact, and it keeps generated/unparsed files
				// (including prowl's own init artifacts) out of the way. --all
				// lists every changed path.
				if !indexed && !all {
					continue
				}
				cf := ChangedFile{File: f, Indexed: indexed}
				if indexed {
					if sum, err := q.BlastSummarize(f); err == nil {
						cf.Impact = &ChangedImpact{
							Total:       sum.Total,
							Direct:      sum.Direct,
							BySubsystem: sum.BySubsystem,
							DirectFiles: sum.DirectFiles,
						}
					}
				}
				res.Files = append(res.Files, cf)
			}
			_ = s.RecordAnswer(res)
			str, err := formatValue(res, format)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), str)
			return err
		},
	}
	c.Flags().StringVar(&base, "base", "HEAD", "git ref to compare against (e.g. main)")
	c.Flags().BoolVar(&all, "all", false, "include changed files prowl does not index")
	return c
}
