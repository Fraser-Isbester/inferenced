package vertex_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/gemini"
	"github.com/Fraser-Isbester/inferenced/internal/provider/vertex"
)

func TestVertex(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Vertex Provider Suite")
}

const (
	geminiPath    = "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-2.5-pro:streamGenerateContent"
	claudePath    = "/v1/projects/p1/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict"
	claudeSync    = "/v1/projects/p1/locations/global/publishers/anthropic/models/claude-opus-4-1@20250805:rawPredict"
	countPath     = "/v1/projects/p1/locations/us-east5/publishers/anthropic/models/count-tokens:rawPredict"
	openapiPath   = "/v1/projects/p1/locations/us-central1/endpoints/openapi/chat/completions"
	mistralPath   = "/v1/projects/p1/locations/us-central1/publishers/mistralai/models/mistral-large-2411:rawPredict"
	geminiStream  = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":7}}\n\n"
	claudeStream  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":20,\"output_tokens\":1}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n"
	openaiStream  = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"
	claudeReqBody = `{"anthropic_version":"vertex-2023-10-16","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`
)

// gateway builds a request as it arrives in env-override mode: the Host is the
// gateway itself, so only the path identifies Vertex.
func gateway(path, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8766"+path, strings.NewReader(body))
}

func parse(p provider.Provider, r *http.Request, ct, stream string) provider.Usage {
	sp := provider.StreamParserFor(p, r, ct)
	_, _ = io.Copy(sp, strings.NewReader(stream))
	return sp.Finish()
}

var _ = Describe("Detection", func() {
	reg := func() *provider.Registry {
		r := provider.NewRegistry()
		r.Register(vertex.New())
		r.Register(anthropic.New())
		r.Register(gemini.New())
		return r
	}

	DescribeTable("claims the regional and global host family in transparent mode",
		func(host string, want bool) {
			r := httptest.NewRequest(http.MethodPost, "https://"+host+"/v1/anything", nil)
			p, ok := reg().Detect(r)
			if want {
				Expect(ok).To(BeTrue())
				Expect(p.Name()).To(Equal("vertex"))
			} else if ok {
				Expect(p.Name()).NotTo(Equal("vertex"))
			}
		},
		Entry("regional", "us-central1-aiplatform.googleapis.com", true),
		Entry("regional with port", "europe-west4-aiplatform.googleapis.com:443", true),
		Entry("global", "aiplatform.googleapis.com", true),
		Entry("not a region label", "evil.example-aiplatform.googleapis.com", false),
		Entry("generative language API", "generativelanguage.googleapis.com", false),
	)

	It("wins the Matches fallback over gemini and anthropic for Vertex paths", func() {
		for _, path := range []string{geminiPath, claudePath, openapiPath} {
			r := gateway(path, "{}")
			r.Header.Set("anthropic-version", "vertex-2023-10-16")
			p, ok := reg().Detect(r)
			Expect(ok).To(BeTrue(), path)
			Expect(p.Name()).To(Equal("vertex"), path)
		}
	})

	It("does not claim non-Vertex traffic", func() {
		for _, path := range []string{"/v1/messages", "/v1beta/models/gemini-2.5-pro:generateContent", "/v1/chat/completions"} {
			Expect(vertex.New().Matches(gateway(path, "{}"))).To(BeFalse(), path)
		}
	})
})

var _ = Describe("UpstreamFor", func() {
	It("routes by the region in the path when the inbound host is the gateway", func() {
		Expect(provider.UpstreamHostFor(vertex.New(), gateway(claudePath, ""))).To(Equal("us-east5-aiplatform.googleapis.com"))
		Expect(provider.UpstreamHostFor(vertex.New(), gateway(claudeSync, ""))).To(Equal("aiplatform.googleapis.com"))
	})

	It("keeps the inbound Vertex host in transparent mode", func() {
		r := httptest.NewRequest(http.MethodPost, "https://europe-west4-aiplatform.googleapis.com"+claudePath, nil)
		Expect(provider.UpstreamHostFor(vertex.New(), r)).To(Equal("europe-west4-aiplatform.googleapis.com"))
	})

	It("never builds a host from an unconstrained path segment", func() {
		r := gateway("/v1/projects/p/locations/evil.com%2Fx/publishers/google/models/m:generateContent", "")
		Expect(provider.UpstreamHostFor(vertex.New(), r)).To(HaveSuffix("aiplatform.googleapis.com"))
		Expect(provider.UpstreamHostFor(vertex.New(), r)).NotTo(ContainSubstring("evil"))
	})

	It("honours a pinned host", func() {
		Expect(provider.UpstreamHostFor(vertex.NewWithHost("psc.example.internal"), gateway(claudePath, ""))).
			To(Equal("psc.example.internal"))
	})
})

var _ = Describe("Gemini on Vertex", func() {
	It("reads the model from the path and meters usageMetadata", func() {
		r := gateway(geminiPath, `{"contents":[{"parts":[{"text":"hello world!"}]}]}`)
		info := vertex.New().ParseRequest(r, []byte(`{"contents":[{"parts":[{"text":"hello world!"}]}]}`))
		Expect(info.Model).To(Equal("gemini-2.5-pro"))
		Expect(info.Stream).To(BeTrue())

		u := parse(vertex.New(), r, "text/event-stream", geminiStream)
		Expect(u).To(Equal(provider.Usage{InputTokens: 12, OutputTokens: 7}))
	})
})

var _ = Describe("Claude on Vertex", func() {
	It("takes the model from the path, since the body has none", func() {
		info := vertex.New().ParseRequest(gateway(claudePath, ""), []byte(claudeReqBody))
		Expect(info.Model).To(Equal("claude-sonnet-4-5@20250929"))
		Expect(info.Stream).To(BeTrue())
		Expect(info.ApproxInputTokens).To(BeNumerically(">", 0))

		info = vertex.New().ParseRequest(gateway(claudeSync, ""), []byte(claudeReqBody))
		Expect(info.Stream).To(BeFalse())
	})

	It("meters the Anthropic SSE usage", func() {
		u := parse(vertex.New(), gateway(claudePath, ""), "text/event-stream", claudeStream)
		Expect(u).To(Equal(provider.Usage{InputTokens: 20, OutputTokens: 9}))
	})

	It("downgrades by rewriting the path and leaves the body alone", func() {
		r := gateway(claudePath, "")
		out, err := vertex.New().RewriteModel(r, []byte(claudeReqBody), "claude-haiku-4-5@20251001")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal(claudeReqBody))
		Expect(r.URL.Path).To(HaveSuffix("/models/claude-haiku-4-5@20251001:streamRawPredict"))
		Expect(vertex.New().ParseRequest(r, out).Model).To(Equal("claude-haiku-4-5@20251001"))
	})

	It("records count-tokens calls at zero cost", func() {
		r := gateway(countPath, "")
		info := vertex.New().ParseRequest(r, []byte(claudeReqBody))
		Expect(info.ApproxInputTokens).To(Equal(0))
		u := parse(vertex.New(), r, "application/json", `{"input_tokens":20}`)
		Expect(u.InputTokens).To(Equal(0))
		Expect(vertex.New().Price(info.Model, 20, 0)).To(BeZero())
	})
})

var _ = Describe("OpenAI-wire on Vertex", func() {
	It("meters the OpenAI-compatible endpoint with the model from the body", func() {
		body := `{"model":"meta/llama-3.3-70b-instruct-maas","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		r := gateway(openapiPath, body)
		Expect(vertex.New().ParseRequest(r, []byte(body)).Model).To(Equal("meta/llama-3.3-70b-instruct-maas"))
		Expect(parse(vertex.New(), r, "text/event-stream", openaiStream)).
			To(Equal(provider.Usage{InputTokens: 30, OutputTokens: 4}))

		out, err := vertex.New().RewriteModel(r, []byte(body), "meta/llama-3.1-8b-instruct-maas")
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(ContainSubstring(`"model":"meta/llama-3.1-8b-instruct-maas"`))
		Expect(r.URL.Path).To(Equal(openapiPath))
	})

	It("rewrites both path and body for MaaS rawPredict that names the model twice", func() {
		body := `{"model":"mistral-large-2411","messages":[]}`
		r := gateway(mistralPath, body)
		out, err := vertex.New().RewriteModel(r, []byte(body), "mistral-small-2503")
		Expect(err).NotTo(HaveOccurred())
		Expect(r.URL.Path).To(HaveSuffix("/models/mistral-small-2503:rawPredict"))
		var got map[string]any
		Expect(json.Unmarshal(out, &got)).To(Succeed())
		Expect(got["model"]).To(Equal("mistral-small-2503"))
	})
})

var _ = Describe("Request-less parser fallback", func() {
	It("picks the authoritative wire from the bytes", func() {
		for stream, want := range map[string]provider.Usage{
			claudeStream: {InputTokens: 20, OutputTokens: 9},
			geminiStream: {InputTokens: 12, OutputTokens: 7},
			openaiStream: {InputTokens: 30, OutputTokens: 4},
		} {
			sp := vertex.New().NewStreamParser("text/event-stream")
			_, _ = io.Copy(sp, strings.NewReader(stream))
			Expect(sp.Finish()).To(Equal(want))
		}
	})
})

var _ = Describe("Price and errors", func() {
	It("prices Claude and Gemini families", func() {
		p := vertex.New()
		Expect(p.Price("claude-opus-4-1@20250805", 1_000_000, 0)).To(BeNumerically("~", 15.0, 1e-9))
		Expect(p.Price("gemini-2.5-flash", 1_000_000, 0)).To(BeNumerically("~", 0.075, 1e-9))
	})

	It("blocks with a Google FAILED_PRECONDITION envelope", func() {
		ct, body := vertex.New().ErrorResponse(http.StatusPaymentRequired, "budget exceeded")
		Expect(ct).To(Equal("application/json"))
		Expect(string(body)).To(ContainSubstring(`"FAILED_PRECONDITION"`))
	})
})
