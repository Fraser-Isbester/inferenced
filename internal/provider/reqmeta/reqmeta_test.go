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

	It("routes tool uses to read/edit/create by classified operation", func() {
		b := reqmeta.New()
		b.AddToolUse("Read", map[string]any{"file_path": "a.go"})
		b.AddToolUse("Edit", map[string]any{"file_path": "b.go"})
		b.AddToolUse("Edit", map[string]any{"file_path": "b.go"}) // dedup file, second edit still counts
		b.AddToolUse("Write", map[string]any{"file_path": "c.go"})
		b.AddToolUse("Bash", map[string]any{"command": "ls"})

		m := b.Map()
		Expect(m["files_read"]).To(ConsistOf("a.go"))
		Expect(m["files_edited"]).To(ConsistOf("b.go"))
		Expect(m["files_created"]).To(ConsistOf("c.go"))
		Expect(m["edits"]).To(Equal(2))
		Expect(m["commands"]).To(ConsistOf("ls"))
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("Edit", 2))
	})

	It("classifies commands into activity counters", func() {
		b := reqmeta.New()
		b.AddCommand("git commit -m wip")
		b.AddCommand("git push origin main")
		b.AddCommand("gh pr create --fill")
		b.AddCommand("go test ./...")
		b.AddCommand("go build -o bin/x ./cmd/x")
		b.AddCommand("git show --stat && echo done") // no commit/build/test

		m := b.Map()
		Expect(m["git_commits"]).To(Equal(1))
		Expect(m["git_pushes"]).To(Equal(1))
		Expect(m["prs_opened"]).To(Equal(1))
		Expect(m["tests_run"]).To(Equal(1))
		Expect(m["builds"]).To(Equal(1))
	})

	It("counts tool failures from error results", func() {
		b := reqmeta.New()
		b.AddToolResult(false)
		b.AddToolResult(true)
		b.AddToolResult(true)
		Expect(b.Map()).To(HaveKeyWithValue("tool_failures", 2))
	})

	It("records the tool-catalog size, not the catalog", func() {
		b := reqmeta.New()
		b.SetToolsAvailable(250)
		m := b.Map()
		Expect(m).To(HaveKeyWithValue("tools_available", 250))
		Expect(m).NotTo(HaveKey("tools"))
	})

	It("ignores unknown tools with no command (no file-list pollution)", func() {
		b := reqmeta.New()
		b.AddToolUse("WebSearch", map[string]any{"query": "otters"})
		m := b.Map()
		Expect(m).NotTo(HaveKey("files_read"))
		Expect(m).To(HaveKeyWithValue("tool_call_counts", HaveKeyWithValue("WebSearch", 1)))
	})

	It("caps unbounded file lists", func() {
		b := reqmeta.New()
		for i := range 500 {
			b.AddFileRead("f" + itoa(i))
		}
		files, ok := b.Map()["files_read"].([]string)
		Expect(ok).To(BeTrue())
		Expect(len(files)).To(Equal(200)) // maxList
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
