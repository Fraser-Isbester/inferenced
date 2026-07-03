package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/store"
)

// defaultRollingWindow is the trailing window used by the rolling period when
// --window is not supplied.
const defaultRollingWindow = 7 * 24 * time.Hour

// init registers the spend command.
func init() { register(newSpendCmd) }

// newSpendCmd builds the "spend" command, which reads the local ledger and
// prints a grouped spend report for the selected period.
func newSpendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spend",
		Short: "Summarize recorded spend from the local ledger",
		RunE: func(cmd *cobra.Command, _ []string) error {
			configPath, _ := cmd.Flags().GetString("config")
			period, _ := cmd.Flags().GetString("period")
			window, _ := cmd.Flags().GetDuration("window")
			since, _ := cmd.Flags().GetString("since")
			return runSpend(cmd, configPath, period, window, since)
		},
	}
	cmd.Flags().String("period", string(periodMonth), "reporting period: day, month, or rolling")
	cmd.Flags().Duration("window", defaultRollingWindow, "trailing window for the rolling period (e.g. 168h)")
	cmd.Flags().String("since", "", "explicit period start override (RFC3339 timestamp, YYYY-MM-DD date, or Go duration like 168h)")
	return cmd
}

// runSpend loads the config, resolves the ledger path, computes the period
// start, queries the report, and prints it. It never panics; failures are
// returned as errors for cobra to surface.
func runSpend(cmd *cobra.Command, configPath, period string, window time.Duration, since string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	start, err := PeriodStart(period, window, since, time.Now())
	if err != nil {
		return err
	}

	path, err := storagePath(cfg)
	if err != nil {
		return err
	}

	st, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer func() { _ = st.Close() }()

	report, err := st.SpendReport(start)
	if err != nil {
		return fmt.Errorf("build spend report: %w", err)
	}

	if _, err := fmt.Fprint(cmd.OutOrStdout(), FormatReport(report, period, start)); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

// storagePath resolves the ledger file location: the configured path when set,
// otherwise the shared default location.
func storagePath(cfg *config.Config) (string, error) {
	if cfg.Storage.Path != "" {
		return cfg.Storage.Path, nil
	}
	path, err := store.DefaultPath()
	if err != nil {
		return "", fmt.Errorf("resolve default ledger path: %w", err)
	}
	return path, nil
}

// period mirrors the enforce package's period identifiers so the CLI computes
// the same window boundaries without importing enforce for these string labels.
type period string

const (
	periodDay     period = "day"
	periodMonth   period = "month"
	periodRolling period = "rolling"
)

// PeriodStart returns the inclusive lower bound for a spend report. When since
// is non-empty it overrides period and is parsed as an RFC3339 timestamp, a
// YYYY-MM-DD date (interpreted in now's location), or a Go duration measured
// back from now. Otherwise the start is computed the same way the enforcement
// engine does: day is the local midnight, month is the local first of the
// month, and rolling is now minus window.
func PeriodStart(p string, window time.Duration, since string, now time.Time) (time.Time, error) {
	if since != "" {
		return parseSince(since, now)
	}
	switch period(p) {
	case periodDay:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()), nil
	case periodMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()), nil
	case periodRolling:
		return now.Add(-window), nil
	default:
		return time.Time{}, fmt.Errorf("invalid period %q (want day, month, or rolling)", p)
	}
}

// parseSince interprets the --since override, trying an RFC3339 timestamp, then
// a date, then a trailing Go duration relative to now.
func parseSince(since string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", since, now.Location()); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(since); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want RFC3339, YYYY-MM-DD, or a duration like 168h)", since)
}

// FormatReport renders a spend report as a readable multi-section table with
// developer, team, and model breakdowns followed by the overall totals. It is a
// pure function so the output is unit-testable without touching a database.
func FormatReport(r store.Report, p string, since time.Time) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)

	// The underlying strings.Builder never returns a write error, so writef
	// discards it; this keeps the many section writes readable while still
	// satisfying strict error handling.
	writef := func(format string, a ...any) { _, _ = fmt.Fprintf(tw, format, a...) }

	writef("inferenced spend\tperiod: %s (since %s)\n", p, since.Format("2006-01-02 15:04"))
	writef("\n")

	writeSection(writef, "By developer", "DEVELOPER", r.ByDeveloper)
	writeSection(writef, "By team", "TEAM", r.ByTeam)
	writeSection(writef, "By model", "MODEL", r.ByModel)

	writef("TOTAL\t$%.4f\t%d requests\n", r.TotalUSD, r.TotalRequests)

	_ = tw.Flush()
	return b.String()
}

// writeSection renders one grouped breakdown with a header row, or a "(none)"
// placeholder when the section has no rows. writef is the buffered writer from
// FormatReport.
func writeSection(writef func(string, ...any), title, keyHeader string, rows []store.ReportRow) {
	writef("%s\t\t\n", title)
	writef("  %s\tCOST\tREQUESTS\n", keyHeader)
	if len(rows) == 0 {
		writef("  (none)\t\t\n")
	}
	for _, row := range rows {
		key := row.Key
		if key == "" {
			key = "(unattributed)"
		}
		writef("  %s\t$%.4f\t%d\n", key, row.CostUSD, row.Requests)
	}
	writef("\n")
}
