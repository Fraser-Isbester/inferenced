package telemetry_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

var _ = Describe("ParseTraceparent", func() {
	It("parses a valid version-00 header and lowercases the ids", func() {
		tid, sid, ok := telemetry.ParseTraceparent("00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-01")
		Expect(ok).To(BeTrue())
		Expect(tid).To(Equal("4bf92f3577b34da6a3ce929d0e0e4736"))
		Expect(sid).To(Equal("00f067aa0ba902b7"))
	})

	It("tolerates surrounding whitespace", func() {
		_, _, ok := telemetry.ParseTraceparent("  00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01  ")
		Expect(ok).To(BeTrue())
	})

	DescribeTable("rejects malformed or absent headers",
		func(header string) {
			tid, sid, ok := telemetry.ParseTraceparent(header)
			Expect(ok).To(BeFalse())
			Expect(tid).To(BeEmpty())
			Expect(sid).To(BeEmpty())
		},
		Entry("empty", ""),
		Entry("too few fields", "00-4bf92f3577b34da6a3ce929d0e0e4736"),
		Entry("unsupported version", "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"),
		Entry("short trace id", "00-abc-00f067aa0ba902b7-01"),
		Entry("short span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067-01"),
		Entry("non-hex trace id", "00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01"),
		Entry("all-zero trace id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"),
		Entry("all-zero span id", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01"),
	)
})
