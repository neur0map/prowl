package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/neur0map/prowl/internal/knowledge"
	"github.com/neur0map/prowl/internal/knowledge/okfv01"
	"github.com/neur0map/prowl/internal/parse/extract"
	"github.com/neur0map/prowl/internal/store"
	"github.com/neur0map/prowl/internal/workspace"
)

type knowledgeSummary struct {
	Path        string   `json:"path"`
	ID          string   `json:"id,omitempty"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Status      string   `json:"status,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func newKnowledgeCmd() *cobra.Command {
	command := &cobra.Command{Use: "knowledge", Short: "Manage portable, reviewable project knowledge"}
	command.AddCommand(
		newKnowledgeInitCmd(), newKnowledgeListCmd(), newKnowledgeShowCmd(), newKnowledgeLintCmd(),
		newKnowledgeProposeCmd(), newKnowledgeAcceptCmd(), newKnowledgeRejectCmd(), newKnowledgeExportCmd(),
	)
	return command
}

func knowledgeWorkspace() (*workspace.Workspace, *knowledge.Repository, *knowledge.ReviewInbox, error) {
	ws, err := workspace.Resolve(".")
	if err != nil {
		return nil, nil, nil, err
	}
	repo := knowledge.NewRepository(ws.Knowledge, okfv01.Codec{})
	return ws, repo, knowledge.NewReviewInbox(ws.Proposals, repo), nil
}

func syncKnowledge(ws *workspace.Workspace, repo *knowledge.Repository) error {
	db, err := store.Open(ws.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	return repo.SyncStore(db, ws.Root, time.Now().UTC())
}

func decideKnowledgeProposal(inbox *knowledge.ReviewInbox, id string, action knowledge.DecisionAction, now time.Time) (*knowledge.Proposal, error) {
	state, err := inbox.Describe(id)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(id + "\x00" + string(action) + "\x00" + state.Version))
	result, err := inbox.Decide(context.Background(), knowledge.DecisionRequest{
		ProposalID: id, Action: action, ExpectedVersion: state.Version,
		IdempotencyKey: hex.EncodeToString(sum[:]), PrincipalID: knowledge.LocalPrincipalID,
	}, now)
	if err != nil {
		return nil, err
	}
	return &result.Proposal, nil
}

func newKnowledgeInitCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "init", Short: "Initialize the trackable OKF knowledge bundle",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, repo, _, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			if err := repo.Init(); err != nil {
				return err
			}
			if err := workspace.EnsureDerivedIgnored(ws.Root); err != nil {
				return err
			}
			if err := syncKnowledge(ws, repo); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"knowledge_root": ws.Knowledge, "initialized": true})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Knowledge bundle ready: %s\n", ws.Knowledge)
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newKnowledgeListCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "list", Short: "List accepted knowledge documents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, repo, _, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			docs, err := repo.List()
			if err != nil {
				return err
			}
			summaries := summarizeKnowledge(docs)
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(summaries)
			}
			if len(summaries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No accepted knowledge yet. Add a proposal with 'prowl knowledge propose'.")
				return nil
			}
			for _, summary := range summaries {
				fmt.Fprintf(cmd.OutOrStdout(), "%-12s %-10s %s — %s\n", summary.Type, summary.Status, summary.Path, summary.Title)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newKnowledgeShowCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "show <id-or-path>", Args: cobra.ExactArgs(1), Short: "Show one accepted knowledge document",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, repo, _, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			docs, err := repo.List()
			if err != nil {
				return err
			}
			for _, doc := range docs {
				if doc.Path != args[0] && doc.Prowl.ID != args[0] {
					continue
				}
				if asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"summary": summarizeKnowledge([]*knowledge.Document{doc})[0], "body": string(doc.Body), "resource": doc.Resource, "anchors": doc.Prowl.Anchors})
				}
				data, err := repo.Codec.Marshal(doc)
				if err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return fmt.Errorf("knowledge document not found: %s", args[0])
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newKnowledgeLintCmd() *cobra.Command {
	var asJSON, repair bool
	command := &cobra.Command{
		Use: "lint", Short: "Check links, IDs, temporal ranges, evidence, and anchor freshness",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ws, repo, _, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			var repairs []knowledge.AnchorRepair
			if repair {
				if repairs, err = repo.RepairAnchors(ws.Root, extract.SymbolRange); err != nil {
					return err
				}
			}
			findings, err := repo.Lint(ws.Root, extract.SymbolRange)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					Repairs  []knowledge.AnchorRepair `json:"repairs,omitempty"`
					Findings []knowledge.Finding      `json:"findings"`
				}{Repairs: repairs, Findings: findings})
			}
			for _, item := range repairs {
				fmt.Fprintf(cmd.OutOrStdout(), "[REPAIR ] %-36s %s\n            moved to lines %d-%d\n", "knowledge.moved_anchor", item.Document, item.ToStart, item.ToEnd)
			}
			if len(findings) == 0 {
				if len(repairs) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "Knowledge health: no findings.")
				}
				return nil
			}
			for _, finding := range findings {
				fmt.Fprintf(cmd.OutOrStdout(), "[%-7s] %-36s %s\n            %s\n", strings.ToUpper(finding.Severity), finding.Code, finding.Path, finding.Message)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	command.Flags().BoolVar(&repair, "repair", false, "rewrite the line range of anchors whose content moved, then report what remains")
	return command
}

func newKnowledgeProposeCmd() *cobra.Command {
	var file, target, author string
	var asJSON bool
	var kind, title, body, bodyFile, resource string
	var tags, anchorFlags []string
	command := &cobra.Command{
		Use: "propose", Short: "Add a validated candidate to the review inbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			structured := kind != "" || title != "" || body != "" || bodyFile != "" || resource != "" || len(tags) > 0 || len(anchorFlags) > 0
			switch {
			case file == "" && !structured:
				return fmt.Errorf("provide --file, or --type and --title to author a candidate inline")
			case file != "" && structured:
				return fmt.Errorf("use --file or the inline authoring flags, not both")
			}
			if structured {
				if target == "" {
					return fmt.Errorf("--target is required when authoring a candidate inline")
				}
				if bodyFile != "" {
					raw, err := os.ReadFile(bodyFile)
					if err != nil {
						return err
					}
					body = string(raw)
				}
				anchors, err := parseAnchorFlags(anchorFlags)
				if err != nil {
					return err
				}
				candidate, err := okfv01.BuildCandidate(okfv01.CaptureInput{
					Type: kind, Title: title, Body: body, Resource: resource, Tags: tags, Anchors: anchors,
				})
				if err != nil {
					return err
				}
				tmp, err := os.CreateTemp("", ".prowl-candidate-*.md")
				if err != nil {
					return err
				}
				file = tmp.Name()
				defer os.Remove(file)
				if _, err := tmp.Write(candidate); err != nil {
					tmp.Close()
					return err
				}
				if err := tmp.Close(); err != nil {
					return err
				}
			}
			if target == "" {
				target = filepath.Base(file)
			}
			ws, _, inbox, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			proposal, diff, err := inbox.Propose(file, target, author, ws.Root, extract.SymbolRange, time.Now().UTC())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"proposal": proposal, "diff": diff})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Proposal %s (%s)\n%s", proposal.ID, proposal.Operation, diff)
			return nil
		},
	}
	command.Flags().StringVar(&file, "file", "", "candidate OKF Markdown file")
	command.Flags().StringVar(&target, "target", "", "bundle-relative destination path")
	command.Flags().StringVar(&author, "author", "", "proposal author or source")
	command.Flags().StringVar(&kind, "type", "", "candidate type when authoring inline (Decision, Claim, ...)")
	command.Flags().StringVar(&title, "title", "", "candidate title when authoring inline")
	command.Flags().StringVar(&body, "body", "", "candidate body text when authoring inline")
	command.Flags().StringVar(&bodyFile, "body-file", "", "read candidate body from a file when authoring inline")
	command.Flags().StringVar(&resource, "resource", "", "resource evidence URI (e.g. file://path) when authoring inline")
	command.Flags().StringSliceVar(&tags, "tag", nil, "candidate tag when authoring inline (repeatable)")
	command.Flags().StringArrayVar(&anchorFlags, "anchor", nil, "source anchor as path#symbol or path:start-end (repeatable)")
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

// parseAnchorFlags turns --anchor strings into anchors. Each is either
// path#symbol (tracks the symbol) or path:start-end / path:line (a line range).
func parseAnchorFlags(flags []string) ([]knowledge.Anchor, error) {
	anchors := make([]knowledge.Anchor, 0, len(flags))
	for _, raw := range flags {
		if i := strings.LastIndex(raw, "#"); i >= 0 {
			path, symbol := raw[:i], raw[i+1:]
			if path == "" || symbol == "" {
				return nil, fmt.Errorf("invalid anchor %q: want path#symbol", raw)
			}
			anchors = append(anchors, knowledge.Anchor{Path: path, Symbol: symbol})
			continue
		}
		if i := strings.LastIndex(raw, ":"); i >= 0 {
			path, span := raw[:i], raw[i+1:]
			start, end, err := parseLineSpan(span)
			if path == "" || err != nil {
				return nil, fmt.Errorf("invalid anchor %q: want path:start-end", raw)
			}
			anchors = append(anchors, knowledge.Anchor{Path: path, LineStart: start, LineEnd: end})
			continue
		}
		return nil, fmt.Errorf("invalid anchor %q: want path#symbol or path:start-end", raw)
	}
	return anchors, nil
}

func parseLineSpan(span string) (int, int, error) {
	if lo, hi, ok := strings.Cut(span, "-"); ok {
		start, err := strconv.Atoi(lo)
		if err != nil {
			return 0, 0, err
		}
		end, err := strconv.Atoi(hi)
		if err != nil {
			return 0, 0, err
		}
		return start, end, nil
	}
	line, err := strconv.Atoi(span)
	if err != nil {
		return 0, 0, err
	}
	return line, line, nil
}

func newKnowledgeAcceptCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "accept <proposal-id>", Args: cobra.ExactArgs(1), Short: "Accept a reviewed proposal atomically",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, repo, inbox, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			diff, err := inbox.Diff(args[0])
			if err != nil {
				return err
			}
			proposal, err := decideKnowledgeProposal(inbox, args[0], knowledge.DecisionAccept, time.Now().UTC())
			if err != nil {
				return err
			}
			if err := syncKnowledge(ws, repo); err != nil {
				return fmt.Errorf("proposal accepted but derived metadata refresh failed: %w", err)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"proposal": proposal, "diff": diff})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%sAccepted proposal %s into %s.\n", diff, proposal.ID, proposal.TargetPath)
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newKnowledgeRejectCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "reject <proposal-id>", Args: cobra.ExactArgs(1), Short: "Reject a proposal without changing accepted knowledge",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _, inbox, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			proposal, err := decideKnowledgeProposal(inbox, args[0], knowledge.DecisionReject, time.Now().UTC())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(proposal)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Rejected proposal %s.\n", proposal.ID)
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func newKnowledgeExportCmd() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use: "export <directory>", Args: cobra.ExactArgs(1), Short: "Export the complete portable OKF bundle",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, repo, _, err := knowledgeWorkspace()
			if err != nil {
				return err
			}
			destination, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if err := repo.Export(destination); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"exported_to": destination})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Exported knowledge bundle to %s\n", destination)
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return command
}

func summarizeKnowledge(docs []*knowledge.Document) []knowledgeSummary {
	out := make([]knowledgeSummary, 0, len(docs))
	for _, doc := range docs {
		out = append(out, knowledgeSummary{Path: doc.Path, ID: doc.Prowl.ID, Type: doc.Type, Title: doc.Title, Description: doc.Description, Status: doc.Prowl.Status, Tags: doc.Tags})
	}
	return out
}
