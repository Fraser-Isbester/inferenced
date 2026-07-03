package gemini_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/gemini"
)

func TestGemini(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Gemini Provider Suite")
}

var _ = Describe("StreamParser", func() {
	It("extracts the last usageMetadata from an SSE stream", func() {
		const stream = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hel\"}]}}]}\n\n" +
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"lo\"}]}}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}\n\n"
		p := gemini.New().NewStreamParser("text/event-stream")
		_, _ = io.Copy(p, strings.NewReader(stream))
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(10))
		Expect(u.OutputTokens).To(Equal(5))
	})

	It("extracts usageMetadata from a JSON-array stream with nested detail fields", func() {
		const body = `[{"candidates":[{"content":{"parts":[{"text":"x"}]}}]},` +
			`{"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":8,"totalTokenCount":28,"promptTokensDetails":[{"modality":"TEXT","tokenCount":20}]}}]`
		p := gemini.New().NewStreamParser("application/json")
		_, _ = io.WriteString(p, body)
		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(20))
		Expect(u.OutputTokens).To(Equal(8))
	})

	It("approximates output tokens from text when usageMetadata is absent", func() {
		const body = `{"candidates":[{"content":{"parts":[{"text":"12345678"}]}}]}`
		p := gemini.New().NewStreamParser("application/json")
		_, _ = io.WriteString(p, body)
		u := p.Finish()
		Expect(u.Approximated).To(BeTrue())
		Expect(u.OutputTokens).To(Equal(2)) // 8 / 4
	})
})

var _ = Describe("ParseRequest", func() {
	It("reads the model from the URL path and estimates input from contents", func() {
		r := httptest.NewRequest("POST", "/v1beta/models/gemini-1.5-flash:streamGenerateContent", nil)
		info := gemini.New().ParseRequest(r, []byte(`{"contents":[{"parts":[{"text":"12345678"}]}]}`))
		Expect(info.Model).To(Equal("gemini-1.5-flash"))
		Expect(info.ApproxInputTokens).To(Equal(2))
		Expect(info.Stream).To(BeTrue())
	})
})

var _ = Describe("Price", func() {
	It("prices pro and flash families by substring", func() {
		p := gemini.New()
		Expect(p.Price("gemini-1.5-flash", 1_000_000, 1_000_000)).To(BeNumerically("~", 0.375, 1e-9))
		Expect(p.Price("gemini-1.5-pro", 1_000_000, 1_000_000)).To(BeNumerically("~", 6.25, 1e-9))
	})
})

var _ = Describe("Metadata", func() {
	It("declares an https upstream host", func() {
		p := gemini.New()
		Expect(p.UpstreamScheme()).To(Equal("https"))
		Expect(p.UpstreamHost()).To(Equal("generativelanguage.googleapis.com"))
	})
})
