package anthropic_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
)

var _ = Describe("ParseRequest metadata", func() {
	const body = `{
	  "model": "claude-sonnet-4-5",
	  "system": [{"type":"text","text":"You are Claude Code.\nWorking directory: /Users/x/repo\nIs directory a git repo: Yes\nCurrent branch: main"}],
	  "tools": [{"name":"Read"},{"name":"Edit"},{"name":"Write"},{"name":"Bash"}],
	  "messages": [
	    {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/Users/x/repo/main.go"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/Users/x/repo/main.go"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"boom","is_error":true}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Write","input":{"file_path":"/Users/x/repo/new.go"}}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"git commit -m wip"}}]}
	  ]
	}`

	It("extracts catalog size, tool counts, files by op, activity, failures, and git env", func() {
		info := anthropic.New().ParseRequest(nil, []byte(body))
		m := info.Metadata
		Expect(m).NotTo(BeNil())
		Expect(m).To(HaveKeyWithValue("tools_available", 4))
		Expect(m).NotTo(HaveKey("tools")) // no catalog dump
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("Read", 1))
		Expect(m["tool_call_counts"]).To(HaveKeyWithValue("Edit", 1))
		Expect(m["files_read"]).To(ConsistOf("/Users/x/repo/main.go"))
		Expect(m["files_edited"]).To(ConsistOf("/Users/x/repo/main.go"))
		Expect(m["files_created"]).To(ConsistOf("/Users/x/repo/new.go"))
		Expect(m["edits"]).To(Equal(1))
		Expect(m["commands"]).To(ConsistOf("git commit -m wip"))
		Expect(m["git_commits"]).To(Equal(1))
		Expect(m["tool_failures"]).To(Equal(1))
		Expect(m["cwd"]).To(Equal("/Users/x/repo"))
		Expect(m["git_repo"]).To(Equal("/Users/x/repo"))
		Expect(m["git_branch"]).To(Equal("main"))
		Expect(m["message_count"]).To(Equal(6))
	})

	It("returns nil metadata for a plain prompt with no tools", func() {
		info := anthropic.New().ParseRequest(nil, []byte(`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"hi"}]}`))
		// message_count is still captured, so metadata is non-nil but has no tool signals.
		Expect(info.Metadata).To(HaveKeyWithValue("message_count", 1))
		Expect(info.Metadata).NotTo(HaveKey("files"))
		Expect(info.Metadata).NotTo(HaveKey("commands"))
	})
})
