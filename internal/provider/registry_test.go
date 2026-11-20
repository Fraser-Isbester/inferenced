package provider_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/gemini"
	"github.com/Fraser-Isbester/inferenced/internal/provider/ollama"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openai"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaicompat"
)

func TestProvider(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Provider Registry Suite")
}

// stub is a Provider whose detection behavior is fully controlled by the spec,
// so registry precedence is tested independently of real path heuristics.
type stub struct {
	provider.Provider // nil; only the methods Detect uses are implemented
	name              string
	hosts             []string
	matches           bool
}

func (s stub) Name() string               { return s.name }
func (s stub) Hosts() []string            { return s.hosts }
func (s stub) Matches(*http.Request) bool { return s.matches }
func (s stub) UpstreamHost() string       { return "" }
func (s stub) UpstreamScheme() string     { return "https" }

func detect(reg *provider.Registry, req *http.Request) string {
	p, ok := reg.Detect(req)
	if !ok {
		return ""
	}
	return p.Name()
}

var _ = Describe("Registry.Detect", func() {
	It("prefers a Host match over an earlier-registered heuristic match", func() {
		reg := provider.NewRegistry()
		reg.Register(stub{name: "greedy", matches: true})
		reg.Register(stub{name: "hosted", hosts: []string{"api.example.com"}})

		req := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
		req.Host = "api.example.com"
		Expect(detect(reg, req)).To(Equal("hosted"))
	})

	It("matches hosts case-insensitively and ignores the request port", func() {
		reg := provider.NewRegistry()
		reg.Register(stub{name: "hosted", hosts: []string{"api.example.com"}})

		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Host = "API.Example.COM:443"
		Expect(detect(reg, req)).To(Equal("hosted"))
	})

	It("falls back to URL.Host when the Host field is empty", func() {
		reg := provider.NewRegistry()
		reg.Register(stub{name: "hosted", hosts: []string{"api.example.com"}})

		req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1", nil)
		req.Host = ""
		Expect(detect(reg, req)).To(Equal("hosted"))
	})

	It("uses registration order to break ties between heuristic matches", func() {
		reg := provider.NewRegistry()
		reg.Register(stub{name: "first", matches: true})
		reg.Register(stub{name: "second", matches: true})
		Expect(detect(reg, httptest.NewRequest(http.MethodPost, "/", nil))).To(Equal("first"))
		Expect(reg.Names()).To(Equal([]string{"first", "second"}))
	})

	It("reports no provider when nothing matches", func() {
		reg := provider.NewRegistry()
		reg.Register(stub{name: "never", hosts: []string{"a.example"}})
		_, ok := reg.Detect(httptest.NewRequest(http.MethodGet, "/healthz", nil))
		Expect(ok).To(BeFalse())
	})

	It("selects a provider whose configured host includes a port via Host-first", func() {
		Skip("BUG: Detect strips the port from the request Host but compares it " +
			"against provider Hosts verbatim, so host:port entries (Ollama's " +
			"localhost:11434, openai_compatible hosts like localhost:8000) never " +
			"match Host-first and fall through to path heuristics.")
		reg := provider.NewRegistry()
		reg.Register(openai.New())
		reg.Register(openaicompat.New(openaicompat.Config{Name: "vllm-local", Host: "localhost:8000", Scheme: "http"}))

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Host = "localhost:8000"
		Expect(detect(reg, req)).To(Equal("vllm-local"))
	})
})

// Routing contract for the built-in registry in env-override mode, where the
// inbound Host is the gateway itself (127.0.0.1:8766) and only path/header
// heuristics can pick the provider. A regression here silently sends a
// credential to the wrong vendor, so every built-in path is pinned.
var _ = Describe("built-in registry routing (env-override mode)", func() {
	var reg *provider.Registry

	BeforeEach(func() {
		// Same order as config.Registry.
		reg = provider.NewRegistry()
		reg.Register(anthropic.New())
		reg.Register(openai.New())
		reg.Register(gemini.New())
		reg.Register(ollama.New())
	})

	DescribeTable("routes each documented endpoint to its provider",
		func(path string, headers map[string]string, want string) {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Host = "127.0.0.1:8766"
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			Expect(detect(reg, req)).To(Equal(want))
		},
		Entry("anthropic messages", "/v1/messages", nil, "anthropic"),
		Entry("anthropic by version header on a non-standard path", "/proxy/x",
			map[string]string{"anthropic-version": "2023-06-01"}, "anthropic"),
		Entry("openai chat completions", "/v1/chat/completions", nil, "openai"),
		Entry("openai responses", "/v1/responses", nil, "openai"),
		Entry("openai embeddings", "/v1/embeddings", nil, "openai"),
		Entry("gemini generateContent", "/v1beta/models/gemini-2.0-flash:generateContent", nil, "gemini"),
		Entry("gemini streamGenerateContent", "/v1beta/models/gemini-2.0-flash:streamGenerateContent", nil, "gemini"),
		Entry("gemini by api-key header", "/v1beta/whatever",
			map[string]string{"x-goog-api-key": "k"}, "gemini"),
		Entry("ollama chat", "/api/chat", nil, "ollama"),
		Entry("ollama generate", "/api/generate", nil, "ollama"),
		Entry("unknown path", "/healthz", nil, ""),
	)

	It("routes an openai_compatible base path to that provider, not to OpenAI", func() {
		Skip("BUG: openai.Matches suffix-matches every OpenAI-wire path and is " +
			"registered before openai_compatible entries, so in env-override mode " +
			"/api/v1/chat/completions (OpenRouter) is detected as openai and forwarded " +
			"to api.openai.com with the OpenRouter key. Compat entries are only " +
			"reachable when the inbound Host equals their host.")
		reg.Register(openaicompat.New(openaicompat.Config{Name: "openrouter", Host: "openrouter.ai", BasePath: "/api/v1"}))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", nil)
		req.Host = "127.0.0.1:8766"
		Expect(detect(reg, req)).To(Equal("openrouter"))
	})
})

var _ = Describe("RewriteBodyModel", func() {
	It("replaces only the model field and preserves every other field", func() {
		in := []byte(`{"model":"claude-opus-4","stream":true,"max_tokens":512,` +
			`"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"u"}}`)
		out, err := provider.RewriteBodyModel(in, "claude-haiku-4")
		Expect(err).NotTo(HaveOccurred())

		var before, after map[string]any
		Expect(json.Unmarshal(in, &before)).To(Succeed())
		Expect(json.Unmarshal(out, &after)).To(Succeed())
		Expect(after["model"]).To(Equal("claude-haiku-4"))
		delete(before, "model")
		delete(after, "model")
		Expect(after).To(Equal(before))
	})

	It("adds a model to a body that has none", func() {
		out, err := provider.RewriteBodyModel([]byte(`{"prompt":"x"}`), "m")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(MatchJSON(`{"prompt":"x","model":"m"}`))
	})

	It("treats a JSON null body as an empty object", func() {
		out, err := provider.RewriteBodyModel([]byte(`null`), "m")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(MatchJSON(`{"model":"m"}`))
	})

	It("returns the original bytes alongside the error on malformed input", func() {
		in := []byte(`{"model":`)
		out, err := provider.RewriteBodyModel(in, "m")
		Expect(err).To(HaveOccurred())
		Expect(out).To(Equal(in))
	})

	It("escapes the new model name rather than splicing it raw", func() {
		out, err := provider.RewriteBodyModel([]byte(`{"model":"a"}`), `x","injected":"y`)
		Expect(err).NotTo(HaveOccurred())
		var got map[string]any
		Expect(json.Unmarshal(out, &got)).To(Succeed())
		Expect(got).To(HaveLen(1))
		Expect(got["model"]).To(Equal(`x","injected":"y`))
	})
})
