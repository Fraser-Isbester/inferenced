package ollama_test

import (
	"io"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/ollama"
)

func TestOllama(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Ollama Provider Suite")
}

var _ = Describe("StreamParser", func() {
	It("reads eval counts from the final NDJSON object", func() {
		const stream = `{"model":"llama3","message":{"content":"Hi"},"done":false}` + "\n" +
			`{"model":"llama3","done":true,"prompt_eval_count":11,"eval_count":7}` + "\n"
		p := ollama.New().NewStreamParser("application/x-ndjson")
		_, _ = io.Copy(p, strings.NewReader(stream))
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(11))
		Expect(u.OutputTokens).To(Equal(7))
	})

	It("parses a single trailing object with no newline", func() {
		const body = `{"done":true,"prompt_eval_count":20,"eval_count":5}`
		p := ollama.New().NewStreamParser("application/json")
		_, _ = io.WriteString(p, body)
		u := p.Finish()
		Expect(u.InputTokens).To(Equal(20))
		Expect(u.OutputTokens).To(Equal(5))
	})
})

var _ = Describe("ParseRequest", func() {
	It("reads the model and estimates input from chat messages", func() {
		info := ollama.New().ParseRequest(nil, []byte(`{"model":"llama3","messages":[{"content":"12345678"}]}`))
		Expect(info.Model).To(Equal("llama3"))
		Expect(info.ApproxInputTokens).To(Equal(2))
	})
})

var _ = Describe("Metadata", func() {
	It("uses an http upstream and prices local inference at zero", func() {
		p := ollama.New()
		Expect(p.UpstreamScheme()).To(Equal("http"))
		Expect(p.UpstreamHost()).To(Equal("localhost:11434"))
		Expect(p.Price("llama3", 1_000_000, 1_000_000)).To(Equal(0.0))
	})

	It("honors a custom upstream", func() {
		p := ollama.NewWithUpstream("10.0.0.5:11434")
		Expect(p.UpstreamHost()).To(Equal("10.0.0.5:11434"))
	})
})
