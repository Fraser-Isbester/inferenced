package store_test

import (
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/artifact"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

var _ = Describe("artifacts", func() {
	var st *store.Store

	BeforeEach(func() {
		var err error
		st, err = store.Open(filepath.Join(GinkgoT().TempDir(), "art.db"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = st.Close() })
	})

	pr := artifact.Artifact{Kind: "pull_request", Source: "github", URL: "https://github.com/a/b/pull/1", Ref: "#1"}
	commit := artifact.Artifact{Kind: "commit", Source: "git", Ref: "abc1234", Title: "main"}

	It("persists artifacts from an event and lists them", func() {
		st.Emit(telemetry.Event{
			RequestID: "r1", TimestampUnixMs: 1000, DeveloperID: "dev-1", Team: "platform",
			Status: 200, Artifacts: []artifact.Artifact{pr, commit},
		})
		rows, err := st.ListArtifacts(time.UnixMilli(0), 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(2))
		Expect(rows).To(ContainElement(HaveField("URL", "https://github.com/a/b/pull/1")))
		Expect(rows).To(ContainElement(HaveField("Ref", "abc1234")))
		Expect(rows[0].DeveloperID).To(Equal("dev-1"))
	})

	It("dedupes the same artifact seen across multiple requests", func() {
		st.Emit(telemetry.Event{RequestID: "r1", TimestampUnixMs: 1000, DeveloperID: "dev-1", Artifacts: []artifact.Artifact{pr}})
		st.Emit(telemetry.Event{RequestID: "r2", TimestampUnixMs: 2000, DeveloperID: "dev-1", Artifacts: []artifact.Artifact{pr}})
		rows, err := st.ListArtifacts(time.UnixMilli(0), 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].FirstSeenMs).To(Equal(int64(1000))) // first sighting wins
	})

	It("skips unidentifiable artifacts (no url or ref)", func() {
		st.Emit(telemetry.Event{RequestID: "r1", TimestampUnixMs: 1000, Artifacts: []artifact.Artifact{{Kind: "push", Source: "git"}}})
		rows, err := st.ListArtifacts(time.UnixMilli(0), 100)
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(BeEmpty())
	})
})

var _ = Describe("SubjectStats", func() {
	It("aggregates spend per developer and team with the latest client", func() {
		st, err := store.Open(filepath.Join(GinkgoT().TempDir(), "subj.db"))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = st.Close() })

		st.Emit(telemetry.Event{RequestID: "1", TimestampUnixMs: 1000, DeveloperID: "alice", Team: "platform", Client: "Cursor", Status: 200, CostUSD: 2})
		st.Emit(telemetry.Event{RequestID: "2", TimestampUnixMs: 2000, DeveloperID: "alice", Team: "platform", Client: "Claude Code", Status: 200, CostUSD: 3})
		st.Emit(telemetry.Event{RequestID: "3", TimestampUnixMs: 1500, DeveloperID: "bob", Team: "ml", Client: "aider", Status: 200, CostUSD: 5})

		stats, err := st.SubjectStats(time.UnixMilli(0))
		Expect(err).NotTo(HaveOccurred())

		byKey := map[string]store.SubjectStat{}
		for _, s := range stats {
			byKey[s.Kind+":"+s.ID] = s
		}
		Expect(byKey["user:alice"].SpentUSD).To(Equal(5.0))
		Expect(byKey["user:alice"].Client).To(Equal("Claude Code")) // latest wins
		Expect(byKey["user:alice"].Team).To(Equal("platform"))
		Expect(byKey["user:bob"].SpentUSD).To(Equal(5.0))
		Expect(byKey["team:platform"].SpentUSD).To(Equal(5.0))
		Expect(byKey["team:ml"].SpentUSD).To(Equal(5.0))
	})
})
