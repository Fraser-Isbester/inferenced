package cli_test

import (
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/cli"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

var _ = Describe("PeriodStart", func() {
	// A fixed, non-midnight instant so day/month truncation is observable.
	now := time.Date(2026, time.July, 15, 9, 30, 0, 0, time.UTC)

	It("truncates the day period to local midnight", func() {
		start, err := cli.PeriodStart("day", 0, "", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC)))
	})

	It("truncates the month period to the local first of the month", func() {
		start, err := cli.PeriodStart("month", 0, "", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)))
	})

	It("subtracts the window for the rolling period", func() {
		start, err := cli.PeriodStart("rolling", 48*time.Hour, "", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(now.Add(-48 * time.Hour)))
	})

	It("rejects an unknown period", func() {
		_, err := cli.PeriodStart("year", 0, "", now)
		Expect(err).To(HaveOccurred())
	})

	It("honors an RFC3339 --since override", func() {
		start, err := cli.PeriodStart("month", 0, "2026-01-02T03:04:05Z", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)))
	})

	It("honors a YYYY-MM-DD --since override in now's location", func() {
		start, err := cli.PeriodStart("month", 0, "2026-03-04", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(time.Date(2026, time.March, 4, 0, 0, 0, 0, time.UTC)))
	})

	It("honors a duration --since override relative to now", func() {
		start, err := cli.PeriodStart("month", 0, "24h", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(start).To(Equal(now.Add(-24 * time.Hour)))
	})

	It("rejects an unparseable --since override", func() {
		_, err := cli.PeriodStart("month", 0, "not-a-time", now)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("FormatReport", func() {
	var (
		st  *store.Store
		now time.Time
	)

	BeforeEach(func() {
		path := filepath.Join(GinkgoT().TempDir(), "ledger.db")
		var err error
		st, err = store.Open(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(st.Close()).To(Succeed()) })

		now = time.Date(2026, time.July, 2, 12, 0, 0, 0, time.UTC)
	})

	emit := func(dev, team, model string, cost float64) {
		st.Emit(telemetry.Event{
			RequestID:       "req",
			TimestampUnixMs: now.UnixMilli(),
			DeveloperID:     dev,
			Team:            team,
			Provider:        "anthropic",
			Model:           model,
			CostUSD:         cost,
		})
	}

	It("renders every section plus totals from a seeded ledger", func() {
		emit("dev-1", "team-a", "claude-3-5-sonnet", 2.50)
		emit("dev-2", "team-a", "gpt-4o", 1.50)

		report, err := st.SpendReport(now.Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())

		out := cli.FormatReport(report, "month", now.Add(-time.Hour))

		Expect(out).To(ContainSubstring("By developer"))
		Expect(out).To(ContainSubstring("By team"))
		Expect(out).To(ContainSubstring("By model"))
		Expect(out).To(ContainSubstring("dev-1"))
		Expect(out).To(ContainSubstring("dev-2"))
		Expect(out).To(ContainSubstring("team-a"))
		Expect(out).To(ContainSubstring("claude-3-5-sonnet"))
		Expect(out).To(ContainSubstring("gpt-4o"))
		Expect(out).To(ContainSubstring("period: month"))

		// Totals: $4.0000 across 2 requests.
		Expect(out).To(ContainSubstring("$4.0000"))
		Expect(out).To(ContainSubstring("2 requests"))
	})

	It("shows a placeholder for empty sections and zeroed totals", func() {
		report, err := st.SpendReport(now)
		Expect(err).NotTo(HaveOccurred())

		out := cli.FormatReport(report, "day", now)

		Expect(out).To(ContainSubstring("(none)"))
		Expect(out).To(ContainSubstring("$0.0000"))
		Expect(out).To(ContainSubstring("0 requests"))
	})

	It("labels unattributed keys instead of printing a blank cell", func() {
		emit("", "", "claude-3-5-sonnet", 1.00)

		report, err := st.SpendReport(now.Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())

		out := cli.FormatReport(report, "month", now.Add(-time.Hour))
		Expect(out).To(ContainSubstring("(unattributed)"))
	})
})
