package openaiwire_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

func TestOpenAIWire(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OpenAI Wire Suite")
}

var _ = Describe("NewParser", func() {
	It("reads chat-completions usage from the final SSE chunk", func() {
		const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"
		p := openaiwire.NewParser("text/event-stream")
		_, _ = io.Copy(p, strings.NewReader(stream))
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(11))
		Expect(u.OutputTokens).To(Equal(7))
	})

	It("reads responses-API usage from the completed event", func() {
		const stream = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}}\n\n" +
			"data: [DONE]\n\n"
		p := openaiwire.NewParser("text/event-stream")
		_, _ = io.Copy(p, strings.NewReader(stream))
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(12))
		Expect(u.OutputTokens).To(Equal(4))
	})

	It("reads embeddings usage from a buffered JSON body (no completion tokens)", func() {
		const body = `{"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":15,"total_tokens":15}}`
		p := openaiwire.NewParser("application/json")
		_, _ = io.WriteString(p, body)
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(15))
		Expect(u.OutputTokens).To(Equal(0))
	})

	It("approximates output tokens when no usage is present", func() {
		const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n\n" +
			"data: [DONE]\n\n"
		p := openaiwire.NewParser("text/event-stream")
		_, _ = io.Copy(p, strings.NewReader(stream))
		u := p.Finish()
		Expect(u.Approximated).To(BeTrue())
		Expect(u.OutputTokens).To(Equal(2)) // 8 / 4
	})
})

var _ = Describe("ParseRequest", func() {
	It("parses the responses-API input field for the estimate", func() {
		r := httptest.NewRequest("POST", "/v1/responses", nil)
		info := openaiwire.ParseRequest(r, []byte(`{"model":"gpt-4o","input":"12345678","stream":false}`))
		Expect(info.Model).To(Equal("gpt-4o"))
		Expect(info.ApproxInputTokens).To(Equal(2))
		Expect(info.Stream).To(BeFalse())
	})

	It("falls back to the URL deployment when the body omits the model (Azure)", func() {
		r := httptest.NewRequest("POST", "/openai/deployments/my-deploy/chat/completions?api-version=2024-02-01", nil)
		info := openaiwire.ParseRequest(r, []byte(`{"messages":[{"role":"user","content":"hi"}]}`))
		Expect(info.Model).To(Equal("my-deploy"))
	})
})
