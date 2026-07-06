package openaiwire_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/artifact"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

var _ = Describe("ParseRequest metadata", func() {
	It("extracts metadata and pairs tool_calls with results to derive artifacts", func() {
		const body = `{
		  "model": "gpt-4o",
		  "tools": [{"type":"function","function":{"name":"read_file"}},{"type":"function","function":{"name":"bash"}}],
		  "messages": [
		    {"role":"user","content":"do it"},
		    {"role":"assistant","tool_calls":[
		      {"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"file_path\":\"cmd/main.go\"}"}},
		      {"id":"c2","type":"function","function":{"name":"bash","arguments":"{\"command\":\"gh pr create --fill\"}"}}
		    ]},
		    {"role":"tool","tool_call_id":"c2","content":"https://github.com/acme/app/pull/7\n"}
		  ]
		}`
		info := openaiwire.ParseRequest(nil, []byte(body))
		m := info.Metadata
		Expect(m).NotTo(BeNil())
		Expect(m).To(HaveKeyWithValue("tools_available", 2))
		Expect(m).NotTo(HaveKey("tools"))
		Expect(m).NotTo(HaveKey("prs_opened")) // classification moved to artifacts
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("read_file", 1))
		Expect(m["files_read"]).To(ConsistOf("cmd/main.go"))
		Expect(m["commands"]).To(ConsistOf("gh pr create --fill"))
		Expect(m["message_count"]).To(Equal(3))

		// The PR link comes from the paired tool result, not the command.
		Expect(info.Artifacts).To(ConsistOf(artifact.Artifact{
			Kind: "pull_request", Source: "github",
			URL: "https://github.com/acme/app/pull/7", Ref: "#7",
		}))
	})
})
