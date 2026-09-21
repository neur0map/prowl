package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/neur0map/prowl/internal/application"
	"github.com/neur0map/prowl/internal/boundedio"
	"github.com/neur0map/prowl/internal/review"
	"github.com/spf13/cobra"
)

const maxReviewReportBytes = 16 << 20

var errReviewReportTooLarge = errors.New("review report exceeds 16 MiB byte limit")

func newReviewCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "review",
		Short: "Plan, inspect, and validate a native change review",
		Args:  cobra.NoArgs,
	}
	command.AddCommand(newReviewPlanCmd(), newReviewUnitCmd(), newReviewCheckCmd())
	return command
}

func newReviewPlanCmd() *cobra.Command {
	var base, head, commit string
	var structured bool
	command := &cobra.Command{
		Use:   "plan",
		Short: "Capture and partition a workspace, commit, or branch-range change",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (err error) {
			request := review.PlanRequest{Commit: commit, Base: base, Head: head, ForceStructured: structured}
			if err := request.Validate(); err != nil {
				return err
			}
			format, err := resolveFormat(command, command.OutOrStdout())
			if err != nil {
				return err
			}
			project, err := application.OpenProject(command.Context(), ".", application.Options{})
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, project.Close()) }()

			plan, planErr := project.Review.Plan(command.Context(), request)
			if plan.Schema != "" {
				if outputErr := writeReviewValue(command, plan, format); outputErr != nil {
					return errors.Join(planErr, outputErr)
				}
			}
			return planErr
		},
	}
	command.Flags().StringVar(&base, "base", "", "base ref for a branch range (head defaults to HEAD)")
	command.Flags().StringVar(&head, "head", "", "head ref for a branch range (requires --base)")
	command.Flags().StringVar(&commit, "commit", "", "single non-merge commit ref to review")
	command.Flags().BoolVar(&structured, "structured", false, "use structured review below the mandatory churn threshold")
	return command
}

func newReviewUnitCmd() *cobra.Command {
	var budgetTokens, budgetBytes int
	command := &cobra.Command{
		Use:   "unit <review-id>/<unit-id>",
		Short: "Fetch one bounded persisted review territory",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) (err error) {
			reviewID, unitID, err := parseReviewUnitArgument(args[0])
			if err != nil {
				return err
			}
			if command.Flags().Changed("budget-tokens") && budgetTokens <= 0 {
				return errors.New("review: --budget-tokens must be positive")
			}
			if command.Flags().Changed("budget-bytes") && budgetBytes <= 0 {
				return errors.New("review: --budget-bytes must be positive")
			}
			format, err := resolveFormat(command, command.OutOrStdout())
			if err != nil {
				return err
			}
			project, err := application.OpenProject(command.Context(), ".", application.Options{})
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, project.Close()) }()

			packet, err := project.Review.UnitPacket(command.Context(), reviewID, unitID, review.UnitRequest{
				BudgetTokens: budgetTokens,
				BudgetBytes:  budgetBytes,
			})
			if err != nil {
				return err
			}
			return writeReviewValue(command, packet, format)
		},
	}
	command.Flags().IntVar(&budgetTokens, "budget-tokens", 0, "optional-context token budget")
	command.Flags().IntVar(&budgetBytes, "budget-bytes", 0, "optional-context byte budget")
	return command
}

func newReviewCheckCmd() *cobra.Command {
	var reviewID, reportPath string
	command := &cobra.Command{
		Use:   "check --review <review-id> --report <regular-file|->",
		Short: "Validate a canonical report against a persisted review plan",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) (err error) {
			if !validReviewPublicID(reviewID) {
				return fmt.Errorf("review: malformed review id %q", reviewID)
			}
			if reportPath == "" {
				return errors.New("review: --report is required")
			}
			format, err := resolveFormat(command, command.OutOrStdout())
			if err != nil {
				return err
			}
			report, err := readReviewReport(command.Context(), reportPath, command.InOrStdin())
			if err != nil {
				return err
			}
			project, err := application.OpenProject(command.Context(), ".", application.Options{})
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, project.Close()) }()

			result, checkErr := project.Review.Check(command.Context(), reviewID, report)
			if result.Schema != "" {
				if outputErr := writeReviewValue(command, result, format); outputErr != nil {
					return errors.Join(checkErr, outputErr)
				}
			}
			return checkErr
		},
	}
	command.Flags().StringVar(&reviewID, "review", "", "persisted review ID")
	command.Flags().StringVar(&reportPath, "report", "", "canonical report JSON regular file, or - for stdin")
	return command
}

func writeReviewValue(command *cobra.Command, value any, format outputFormat) error {
	output, err := formatValue(value, format)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(command.OutOrStdout(), output)
	return err
}

func parseReviewUnitArgument(value string) (string, string, error) {
	reviewID, unitID, ok := strings.Cut(value, "/")
	if !ok || strings.Contains(unitID, "/") || !validReviewPublicID(reviewID) || !validUnitPublicID(unitID) {
		return "", "", fmt.Errorf("review: malformed review unit id %q (want <review-id>/<unit-id>)", value)
	}
	return reviewID, unitID, nil
}

func validReviewPublicID(value string) bool {
	return validPrefixedLowerHex(value, review.ReviewIDPrefixV1, 40)
}

func validUnitPublicID(value string) bool {
	return validPrefixedLowerHex(value, review.UnitIDPrefixV1, 32)
}

func validPrefixedLowerHex(value, prefix string, digits int) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	digest := value[len(prefix):]
	if len(digest) != digits {
		return false
	}
	for index := range digest {
		if (digest[index] < '0' || digest[index] > '9') && (digest[index] < 'a' || digest[index] > 'f') {
			return false
		}
	}
	return true
}

func readReviewReport(ctx context.Context, path string, stdin io.Reader) (review.Report, error) {
	if path == "-" {
		return decodeReviewReport(ctx, stdin)
	}
	data, err := readRegularReviewReport(ctx, path)
	if err != nil {
		return review.Report{}, err
	}
	return decodeReviewReportBytes(ctx, data)
}

func readRegularReviewReport(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("review: resolve report path: %w", err)
	}
	volume := filepath.VolumeName(absolute)
	rootPath := volume + string(filepath.Separator)
	relative, err := filepath.Rel(rootPath, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("review: invalid report path %q", path)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("review: open report filesystem root: %w", err)
	}
	defer root.Close()
	file, err := boundedio.OpenRegularNoFollow(root, relative)
	if err != nil {
		return nil, fmt.Errorf("review: report must be a regular file opened without symlinks: %w", err)
	}
	defer file.Close()
	data, err := boundedio.ReadAllContext(ctx, file, maxReviewReportBytes+1)
	if errors.Is(err, boundedio.ErrTooLarge) || len(data) > maxReviewReportBytes {
		return nil, errReviewReportTooLarge
	}
	if err != nil {
		return nil, fmt.Errorf("review: read report: %w", err)
	}
	return data, nil
}

func decodeReviewReport(ctx context.Context, input io.Reader) (review.Report, error) {
	if input == nil {
		return review.Report{}, errors.New("review: report input is unavailable")
	}
	limited := &io.LimitedReader{R: &reviewContextReader{ctx: ctx, reader: input}, N: maxReviewReportBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return review.Report{}, fmt.Errorf("review: read report: %w", err)
	}
	if len(data) > maxReviewReportBytes {
		return review.Report{}, errReviewReportTooLarge
	}
	return decodeReviewReportBytes(ctx, data)
}

func decodeReviewReportBytes(ctx context.Context, data []byte) (review.Report, error) {
	if err := ctx.Err(); err != nil {
		return review.Report{}, err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return review.Report{}, errors.New("review: report JSON must contain one object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var report review.Report
	if err := decoder.Decode(&report); err != nil {
		return review.Report{}, fmt.Errorf("review: decode report JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return review.Report{}, errors.New("review: trailing data after report JSON object")
		}
		return review.Report{}, fmt.Errorf("review: trailing data after report JSON object: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return review.Report{}, err
	}
	return report, nil
}

type reviewContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *reviewContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
