package reqmeta_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/reqmeta"
)

func TestReqmeta(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Reqmeta Suite")
}

var _ = Describe("Builder", func() {
	It("returns nil when nothing is captured", func() {
		Expect(reqmeta.New().Map()).To(BeNil())
	})

	It("dedupes files and commands and counts tool calls", func() {
		b := reqmeta.New()
		b.AddFile("a.go")
		b.AddFile("a.go")
		b.AddFile("b.go")
		b.AddCommand("git status")
		b.AddToolCall("Read")
		b.AddToolCall("Read")
		b.SetMessageCount(4)

		m := b.Map()
		Expect(m["files"]).To(ConsistOf("a.go", "b.go"))
		Expect(m["commands"]).To(ConsistOf("git status"))
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("Read", 2))
		Expect(m["message_count"]).To(Equal(4))
	})

	It("caps unbounded lists", func() {
		b := reqmeta.New()
		for i := range 500 {
			b.AddFile("f" + itoa(i))
		}
		files, ok := b.Map()["files"].([]string)
		Expect(ok).To(BeTrue())
		Expect(len(files)).To(Equal(200)) // maxList
	})

	It("ignores empty values", func() {
		b := reqmeta.New()
		b.AddFile("")
		b.AddCommand("")
		b.AddTool("")
		b.AddToolCall("")
		Expect(b.Map()).To(BeNil())
	})
})

// itoa is a tiny local int->string to avoid importing strconv in the test body.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
