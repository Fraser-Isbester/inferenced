package store_test

import (
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

var _ = Describe("RecentEvents and EventsSince", func() {
	var st *store.Store

	BeforeEach(func() {
		var err error
		st, err = store.Open(filepath.Join(GinkgoT().TempDir(), "events.db"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = st.Close() })

		for i := 1; i <= 3; i++ {
			st.Emit(telemetry.Event{
				RequestID: string(rune('0' + i)), TimestampUnixMs: int64(i),
				Provider: "anthropic", Model: "claude-sonnet-4-5", Status: 200, CostUSD: float64(i),
			})
		}
	})

	It("returns the most recent events ascending with the current max rowid", func() {
		rows, maxRowID, err := st.RecentEvents(2)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(2))
		// Ascending order: the two newest are rows 2 and 3.
		Expect(rows[0].RowID).To(BeNumerically("<", rows[1].RowID))
		Expect(rows[1].RowID).To(Equal(maxRowID))
		Expect(maxRowID).To(Equal(int64(3)))
	})

	It("tails only rows newer than the given rowid", func() {
		rows, maxRowID, err := st.EventsSince(1, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(2)) // rows 2 and 3
		Expect(maxRowID).To(Equal(int64(3)))

		none, max2, err := st.EventsSince(maxRowID, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(none).To(BeEmpty())
		Expect(max2).To(Equal(int64(3))) // unchanged when nothing new

		st.Emit(telemetry.Event{RequestID: "4", TimestampUnixMs: 4, Provider: "openai", Model: "gpt-4o", Status: 200})
		fresh, max3, err := st.EventsSince(maxRowID, 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(fresh).To(HaveLen(1))
		Expect(fresh[0].Provider).To(Equal("openai"))
		Expect(max3).To(Equal(int64(4)))
	})

	It("returns an empty result and zero max on a fresh ledger", func() {
		empty, err := store.Open(filepath.Join(GinkgoT().TempDir(), "empty.db"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = empty.Close() })

		rows, maxRowID, err := empty.RecentEvents(10)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
		Expect(maxRowID).To(Equal(int64(0)))
	})
})
