package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/prowl-agent/prowl-agent/internal/application"
	"github.com/prowl-agent/prowl-agent/internal/config"
	"github.com/prowl-agent/prowl-agent/internal/doctor"
	"github.com/prowl-agent/prowl-agent/internal/setup"
)

// cRedHex is the error accent (Catppuccin red); the shared palette in
// statuscard.go covers the rest.
const cRedHex = "#f38ba8"

func newDoctorCmd(version string) *cobra.Command {
	var asJSON, includeExcluded, integrations bool
	var profile, format, baseline, failOn string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose repository health (use --profile rice for desktop/dotfile checks)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch failOn {
			case "none", "warn", "error":
			default:
				return fmt.Errorf("unknown doctor --fail-on value %q (choose none, warn, or error)", failOn)
			}
			if integrations {
				for _, flag := range []string{"profile", "baseline", "fail-on", "include-excluded"} {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("doctor --integrations cannot be combined with repository-only --%s", flag)
					}
				}
				if format == "sarif" || format == "shields" {
					return fmt.Errorf("doctor --integrations supports only human or json output, not %s", format)
				}
				resolved := format
				if asJSON {
					resolved = "json"
				}
				if resolved != "" && resolved != "human" && resolved != "json" {
					return fmt.Errorf("unknown doctor integrations format %q (choose human or json)", resolved)
				}
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				report, err := setup.VerifyUserIntegrations(setup.UserInstallOptions{
					Home: home, Version: version, Clients: setup.DetectInstalledHarnesses(),
				})
				if err != nil {
					return err
				}
				if resolved == "json" {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(report)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderDoctorIntegrations(report))
				return nil
			}
			if profile != doctor.ProfileGeneral && profile != doctor.ProfileRice {
				return fmt.Errorf("unknown doctor profile %q (choose general or rice)", profile)
			}
			project, err := application.OpenProject(cmd.Context(), ".", application.Options{})
			if err != nil {
				return err
			}
			defer project.Close()
			release, err := project.ReadGuard(cmd.Context())
			if err != nil {
				return err
			}
			defer release()
			if err := project.Store.RequirePublishedGeneration(); err != nil {
				return err
			}
			rules, err := config.LoadRules(project.Workspace.Path)
			if err != nil {
				return err
			}
			rep, err := doctor.Run(project.Store, rules, doctor.Options{Root: project.Workspace.Root, Ignore: project.Config.Ignore, Profile: profile, IncludeExcluded: includeExcluded})
			if err != nil {
				return err
			}
			if baseline != "" {
				base, berr := loadDoctorBaseline(baseline)
				if berr != nil {
					return fmt.Errorf("read baseline %s: %w", baseline, berr)
				}
				rep = deltaReport(rep, base)
			}
			out := cmd.OutOrStdout()
			resolved := format
			if asJSON {
				resolved = "json"
			}
			switch resolved {
			case "json":
				if err := json.NewEncoder(out).Encode(rep); err != nil {
					return err
				}
			case "sarif":
				s, err := renderDoctorSARIF(rep)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, s)
			case "shields":
				s, err := renderDoctorShields(rep)
				if err != nil {
					return err
				}
				fmt.Fprintln(out, s)
			case "", "human":
				if f, ok := out.(*os.File); ok && isTTY(f) {
					fmt.Fprintln(out, renderDoctorCard(rep))
				} else {
					fmt.Fprint(out, renderDoctorPlain(rep))
				}
			default:
				return fmt.Errorf("unknown doctor format %q (choose human, json, sarif, or shields)", resolved)
			}
			// CI gate: exit non-zero when new/kept findings breach the threshold.
			errs, warns, _ := severityCounts(rep.Findings)
			switch failOn {
			case "error":
				if errs > 0 {
					cmd.SilenceUsage = true
					return fmt.Errorf("doctor gate: %d error finding(s)", errs)
				}
			case "warn":
				if errs+warns > 0 {
					cmd.SilenceUsage = true
					return fmt.Errorf("doctor gate: %d error and %d warning finding(s)", errs, warns)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON (alias for --format json)")
	c.Flags().StringVar(&format, "format", "", "output format: human, json, sarif, or shields")
	c.Flags().StringVar(&baseline, "baseline", "", "prior --format json report; report only findings new since it")
	c.Flags().StringVar(&failOn, "fail-on", "none", "exit non-zero when findings reach this severity: none, warn, or error")
	c.Flags().StringVar(&profile, "profile", doctor.ProfileGeneral, "diagnostic profile: general or rice")
	c.Flags().BoolVar(&includeExcluded, "include-excluded", false, "report findings under excluded lifecycle paths (tests/, vendor/, ...) instead of filtering them, including every file with a masked credential")
	c.Flags().BoolVar(&integrations, "integrations", false, "report user-level Claude, OMP, and PATH CLI integration health")
	return c
}

func renderDoctorIntegrations(report setup.UserIntegrationHealth) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent integration health (package %s)\n", report.PackageVersion)
	if report.InstalledPackageVersion != "" {
		fmt.Fprintf(&b, "Installed package: %s\n", report.InstalledPackageVersion)
	}
	fmt.Fprintf(&b, "CLI: %s", report.CLI.Status)
	if report.CLI.Version != "" {
		fmt.Fprintf(&b, " (%s)", report.CLI.Version)
	}
	b.WriteByte('\n')
	for _, client := range report.Clients {
		fmt.Fprintf(&b, "%s: %s", client.Client, client.Status)
		switch client.Activation {
		case setup.UserActivationRestartRequired:
			b.WriteString(" (restart required after install)")
		case setup.UserActivationReloadRequired:
			b.WriteString(" (reload required after install)")
		}
		b.WriteByte('\n')
	}
	for _, asset := range report.Assets {
		if asset.State == setup.UserAssetCurrent {
			continue
		}
		fmt.Fprintf(&b, "asset %s: %s %s", asset.Client, asset.State, asset.Destination)
		if asset.AssetID != "" {
			fmt.Fprintf(&b, " (%s)", asset.AssetID)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// severityCounts tallies findings by severity.
func severityCounts(fs []doctor.Finding) (errs, warns, infos int) {
	for _, f := range fs {
		switch f.Severity {
		case doctor.SevError:
			errs++
		case doctor.SevWarn:
			warns++
		case doctor.SevInfo:
			infos++
		}
	}
	return
}

func findingLoc(f doctor.Finding) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

// renderDoctorPlain is the un-styled report for pipes and non-terminals. It
// leads with the score and a severity breakdown, then one block per finding.
func renderDoctorPlain(rep doctor.Report) string {
	var b strings.Builder
	errs, warns, infos := severityCounts(rep.Findings)
	fmt.Fprintf(&b, "Health score: %d/100\n", rep.Score)
	fmt.Fprintf(&b, "Findings: %d (errors %d, warns %d, info %d)\n", len(rep.Findings), errs, warns, infos)
	if len(rep.Findings) == 0 {
		b.WriteString("No issues found.\n")
		return b.String()
	}
	b.WriteString("\n")
	for _, f := range rep.Findings {
		fmt.Fprintf(&b, "[%-5s] %-20s %s\n          %s\n",
			strings.ToUpper(string(f.Severity)), f.Check, findingLoc(f), f.Detail)
	}
	return b.String()
}

// scoreColor bands the health score: green (good), yellow (mid), red (low).
func scoreColor(score int) string {
	switch {
	case score >= 80:
		return "#a6e3a1"
	case score >= 50:
		return "#f9e2af"
	default:
		return cRedHex
	}
}

func sevTag(s doctor.Severity) string {
	switch s {
	case doctor.SevError:
		return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(cRedHex)).Render("error")
	case doctor.SevWarn:
		return stWarn.Render("warn ")
	default:
		return stFaint.Render("info ")
	}
}

// renderDoctorCard is the bordered, colored report for a terminal: a score bar,
// a severity breakdown, per-check counts, and grouped findings.
func renderDoctorCard(rep doctor.Report) string {
	errs, warns, infos := severityCounts(rep.Findings)
	var L []string
	L = append(L, stTitle.Render("doctor")+"  "+stFaint.Render(fmt.Sprintf("%d findings", len(rep.Findings))))
	L = append(L, "")
	L = append(L, stLabel.Render("HEALTH"))
	L = append(L, "  "+bar(float64(rep.Score)/100, 24, scoreColor(rep.Score))+fmt.Sprintf("  %d/100", rep.Score))
	L = append(L, "  "+lipgloss.NewStyle().Foreground(lipgloss.Color(cRedHex)).Render(fmt.Sprintf("errors %d", errs))+
		"   "+stWarn.Render(fmt.Sprintf("warns %d", warns))+
		"   "+stFaint.Render(fmt.Sprintf("info %d", infos)))

	if len(rep.Summary) > 0 {
		L = append(L, "")
		L = append(L, stLabel.Render("BY CHECK"))
		keys := make([]string, 0, len(rep.Summary))
		for k := range rep.Summary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			L = append(L, "  "+stLabel.Width(24).Render(k)+stNum.Render(strconv.Itoa(rep.Summary[k])))
		}
	}

	L = append(L, "")
	if len(rep.Findings) == 0 {
		L = append(L, stBig.Render("No issues found."))
	} else {
		L = append(L, stLabel.Render("FINDINGS"))
		for _, f := range rep.Findings {
			L = append(L, "  "+sevTag(f.Severity)+"  "+stNum.Render(f.Check)+"  "+stFaint.Render(findingLoc(f)))
			L = append(L, "      "+f.Detail)
		}
	}

	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cFaint).Padding(0, 2)
	return box.Render(lipgloss.JoinVertical(lipgloss.Left, L...))
}
