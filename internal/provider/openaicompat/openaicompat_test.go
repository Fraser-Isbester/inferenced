package openaicompat_test

import (
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaicompat"
)

func TestOpenAICompat(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OpenAI-Compatible Provider Suite")
}

var _ = Describe("compatProvider", func() {
	It("defaults the scheme to https and reports its host", func() {
		p := openaicompat.New(openaicompat.Config{Name: "openrouter", Host: "openrouter.ai"})
		Expect(p.Name()).To(Equal("openrouter"))
		Expect(p.UpstreamHost()).To(Equal("openrouter.ai"))
		Expect(p.UpstreamScheme()).To(Equal("https"))
	})

	It("honors an http scheme for local endpoints", func() {
		p := openaicompat.New(openaicompat.Config{Name: "vllm", Host: "localhost:8000", Scheme: "http"})
		Expect(p.UpstreamScheme()).To(Equal("http"))
	})

	It("matches OpenAI-style paths, scoped to a base path when set", func() {
		p := openaicompat.New(openaicompat.Config{Name: "openrouter", Host: "openrouter.ai", BasePath: "/api/v1"})
		Expect(p.Matches(httptest.NewRequest("POST", "/api/v1/chat/completions", nil))).To(BeTrue())
		Expect(p.Matches(httptest.NewRequest("POST", "/chat/completions", nil))).To(BeFalse())
	})

	It("is resolved by Registry via Host-first detection", func() {
		reg := provider.NewRegistry()
		reg.Register(openaicompat.New(openaicompat.Config{Name: "groq", Host: "api.groq.com"}))
		req := httptest.NewRequest("POST", "/openai/v1/chat/completions", nil)
		req.Host = "api.groq.com"
		got, ok := reg.Detect(req)
		Expect(ok).To(BeTrue())
		Expect(got.Name()).To(Equal("groq"))
	})

	It("prices by substring, then default, then zero", func() {
		p := openaicompat.New(openaicompat.Config{
			Name: "openrouter", Host: "openrouter.ai",
			Pricing: map[string]openaicompat.Price{
				"default":   {InputPer1M: 1, OutputPer1M: 2},
				"llama-3.1": {InputPer1M: 0.5, OutputPer1M: 1},
			},
		})
		Expect(p.Price("meta-llama/llama-3.1-70b", 1_000_000, 1_000_000)).To(BeNumerically("~", 1.5, 1e-9))
		Expect(p.Price("some-other-model", 1_000_000, 1_000_000)).To(BeNumerically("~", 3, 1e-9))

		noPrice := openaicompat.New(openaicompat.Config{Name: "x", Host: "x.example"})
		Expect(noPrice.Price("anything", 1_000_000, 1_000_000)).To(Equal(0.0))
	})
})
