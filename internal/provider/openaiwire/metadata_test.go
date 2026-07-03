package openaiwire_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

var _ = Describe("ParseRequest metadata", func() {
	It("extracts tools, tool-call counts, files, and commands from tool_calls", func() {
		const body = `{
		  "model": "gpt-4o",
		  "tools": [{"type":"function","function":{"name":"read_file"}},{"type":"function","function":{"name":"bash"}}],
		  "messages": [
		    {"role":"user","content":"do it"},
		    {"role":"assistant","tool_calls":[
		      {"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"file_path\":\"cmd/main.go\"}"}},
		      {"id":"c2","type":"function","function":{"name":"bash","arguments":"{\"command\":\"gh pr create\"}"}}
		    ]}
		  ]
		}`
		info := openaiwire.ParseRequest(nil, []byte(body))
		m := info.Metadata
		Expect(m).NotTo(BeNil())
		Expect(m["tools"]).To(ConsistOf("read_file", "bash"))
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("read_file", 1))
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("bash", 1))
		Expect(m["files"]).To(ConsistOf("cmd/main.go"))
		Expect(m["commands"]).To(ConsistOf("gh pr create"))
		Expect(m["message_count"]).To(Equal(2))
	})
})
