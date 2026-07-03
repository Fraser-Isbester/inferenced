package store_test

import (
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

func TestStore(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Store Suite")
}

var _ = Describe("Store", func() {
	var (
		st  *store.Store
		now time.Time
	)

	BeforeEach(func() {
		// A per-spec temp file keeps the suite hermetic: no shared state, no
		// network, and the OS reclaims the file after the test process exits.
		path := filepath.Join(GinkgoT().TempDir(), "ledger.db")
		var err error
		st, err = store.Open(path)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(st.Close()).To(Succeed()) })

		now = time.Date(2026, time.July, 2, 12, 0, 0, 0, time.UTC)
	})

	emit := func(dev, team string, cost float64, ts time.Time) {
		st.Emit(telemetry.Event{
			RequestID:       "req",
			TimestampUnixMs: ts.UnixMilli(),
			DeveloperID:     dev,
			Team:            team,
			Provider:        "anthropic",
			Model:           "claude-3-5-sonnet",
			CostUSD:         cost,
		})
	}

	It("sums spend for a developer since a cutoff", func() {
		emit("dev-1", "team-a", 2.50, now)
		emit("dev-1", "team-a", 1.25, now.Add(time.Hour))
		emit("dev-2", "team-a", 9.99, now) // different developer, excluded

		spent, err := st.Spend("developer", "dev-1", now.Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(spent).To(BeNumerically("~", 3.75, 1e-9))
	})

	It("sums spend for a team", func() {
		emit("dev-1", "team-a", 2.0, now)
		emit("dev-2", "team-a", 3.0, now)
		emit("dev-3", "team-b", 5.0, now)

		spent, err := st.Spend("team", "team-a", now.Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(spent).To(BeNumerically("~", 5.0, 1e-9))
	})

	It("sums all rows for the wildcard id", func() {
		emit("dev-1", "team-a", 1.0, now)
		emit("dev-2", "team-b", 4.0, now)

		spent, err := st.Spend("developer", "*", now.Add(-time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(spent).To(BeNumerically("~", 5.0, 1e-9))
	})

	It("excludes rows older than the cutoff", func() {
		emit("dev-1", "team-a", 10.0, now.Add(-48*time.Hour)) // before cutoff
		emit("dev-1", "team-a", 1.0, now)                     // after cutoff

		spent, err := st.Spend("developer", "dev-1", now.Add(-24*time.Hour))
		Expect(err).NotTo(HaveOccurred())
		Expect(spent).To(BeNumerically("~", 1.0, 1e-9))
	})

	It("returns zero when no rows match", func() {
		spent, err := st.Spend("developer", "nobody", now)
		Expect(err).NotTo(HaveOccurred())
		Expect(spent).To(Equal(0.0))
	})

	It("rejects an unknown scope", func() {
		_, err := st.Spend("bogus", "x", now)
		Expect(err).To(HaveOccurred())
	})
})
