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
