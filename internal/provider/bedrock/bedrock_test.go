package bedrock_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/bedrock"
	"github.com/Fraser-Isbester/inferenced/internal/provider/bedrock/bedrocktest"
)

func TestBedrock(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Bedrock Provider Suite")
}

const (
	host       = "bedrock-runtime.us-west-2.amazonaws.com"
	claudeID   = "us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	claudeBody = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":64,"messages":[{"role":"user","content":"hello there"}]}`
)

// signed builds a request as transparent interception delivers it: the Host is
// the real Bedrock host the client signed for.
func signed(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://"+host+path, bytes.NewBufferString(body))
	r.Host = host
	return r
}

func feed(p provider.StreamParser, chunks ...[]byte) provider.Usage {
	for _, c := range chunks {
		_, _ = p.Write(c)
	}
	return p.Finish()
}

var _ = Describe("Bedrock provider", func() {
	p := bedrock.New()

	Describe("detection and routing", func() {
		It("claims the regional and FIPS Bedrock Runtime hosts by Host only", func() {
			hm := p.(provider.HostMatcher)
			Expect(hm.MatchesHost("bedrock-runtime.us-west-2.amazonaws.com")).To(BeTrue())
			Expect(hm.MatchesHost("BEDROCK-RUNTIME.eu-central-1.amazonaws.com")).To(BeTrue())
			Expect(hm.MatchesHost("bedrock-runtime-fips.us-east-1.amazonaws.com")).To(BeTrue())
			Expect(hm.MatchesHost("bedrock.us-west-2.amazonaws.com")).To(BeFalse(), "control plane, not inference")
			Expect(hm.MatchesHost("bedrock-runtime.us-west-2.amazonaws.com.evil.test")).To(BeFalse())
			Expect(hm.MatchesHost("api.anthropic.com")).To(BeFalse())
		})

		It("never claims env-override traffic, whose signature can't survive forwarding", func() {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8766/model/"+claudeID+"/invoke", nil)
			Expect(p.Matches(r)).To(BeFalse())
		})

		It("is detected through the registry by Host, port and case aside", func() {
			reg := provider.NewRegistry()
			reg.Register(p)
			r := signed("/model/"+claudeID+"/invoke", claudeBody)
			r.Host = "Bedrock-Runtime.us-west-2.amazonaws.com:443"
			got, ok := reg.Detect(r)
			Expect(ok).To(BeTrue())
			Expect(got.Name()).To(Equal("bedrock"))
		})

		It("forwards to the host the client signed for", func() {
			Expect(provider.UpstreamHostFor(p, signed("/model/x/invoke", ""))).To(Equal(host))
		})

		It("forwards the signed Host value verbatim, port and case included", func() {
			r := signed("/model/x/invoke", "")
			r.Host = "Bedrock-Runtime.us-west-2.amazonaws.com:443"
			Expect(provider.UpstreamHostFor(p, r)).To(Equal("Bedrock-Runtime.us-west-2.amazonaws.com:443"))
		})

		It("falls back to the configured region, rejecting a malformed one", func() {
			r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/model/x/invoke", nil)
			Expect(provider.UpstreamHostFor(bedrock.NewWithRegion("eu-west-1"), r)).To(Equal("bedrock-runtime.eu-west-1.amazonaws.com"))
			Expect(bedrock.NewWithRegion("evil.test/x").UpstreamHost()).To(Equal("bedrock-runtime.us-east-1.amazonaws.com"))
		})
	})

	Describe("ParseRequest", func() {
		It("reads the model from the path and the Anthropic body's estimate", func() {
			info := p.ParseRequest(signed("/model/"+claudeID+"/invoke-with-response-stream", claudeBody), []byte(claudeBody))
			Expect(info.Model).To(Equal(claudeID))
			Expect(info.Stream).To(BeTrue())
			Expect(info.ApproxInputTokens).To(BeNumerically(">", 0))
		})

		It("keeps the model id of an inference-profile ARN, not the account", func() {
			arn := "arn%3Aaws%3Abedrock%3Aus-west-2%3A123456789012%3Ainference-profile%2F" + claudeID
			info := p.ParseRequest(signed("/model/"+arn+"/invoke", claudeBody), []byte(claudeBody))
			Expect(info.Model).To(Equal(claudeID))
		})

		It("estimates a Converse request from its text blocks", func() {
			body := `{"system":[{"text":"be brief"}],"messages":[{"role":"user","content":[{"text":"hello world, twelve"}]}]}`
			info := p.ParseRequest(signed("/model/amazon.nova-pro-v1:0/converse", body), []byte(body))
			Expect(info.Model).To(Equal("amazon.nova-pro-v1:0"))
			Expect(info.Stream).To(BeFalse())
			Expect(info.ApproxInputTokens).To(Equal((len("be brief") + len("hello world, twelve")) / 4))
		})

		It("gives a non-inference route, such as count-tokens, no billable estimate", func() {
			body := `{"input":{"invokeModel":{"body":"` + strings.Repeat("x", 400) + `"}}}`
			info := p.ParseRequest(signed("/model/"+claudeID+"/count-tokens", body), []byte(body))
			Expect(info).To(Equal(provider.RequestInfo{}))
		})

		It("estimates a Claude Text Completions request from its prompt", func() {
			prompt := `\n\nHuman: summarize the quarterly report please\n\nAssistant:` // JSON-escaped
			body := `{"prompt":"` + prompt + `","max_tokens_to_sample":200}`
			info := p.ParseRequest(signed("/model/anthropic.claude-v2:1/invoke", body), []byte(body))
			Expect(info.ApproxInputTokens).To(BeNumerically(">", 0))
		})
	})

	Describe("metering", func() {
		It("prefers Bedrock's invocation metrics in an InvokeModel stream", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1}}}`),
				bedrocktest.Chunk(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`),
				bedrocktest.Chunk(`{"type":"message_delta","usage":{"output_tokens":9}}`),
				bedrocktest.Chunk(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":21,"outputTokenCount":10}}`),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 21, OutputTokens: 10}))
		})

		It("uses the Anthropic counts when a stream ends before the metrics", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"type":"message_start","message":{"usage":{"input_tokens":20,"output_tokens":1}}}`),
				bedrocktest.Chunk(`{"type":"message_delta","usage":{"output_tokens":9}}`),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 20, OutputTokens: 9}))
		})

		It("decodes frames split across writes at any byte", func() {
			all := append(bedrocktest.Chunk(`{"type":"message_start","message":{"usage":{"input_tokens":7}}}`),
				bedrocktest.Chunk(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":7,"outputTokenCount":3}}`)...)
			sp := p.NewStreamParser("application/vnd.amazon.eventstream")
			for _, b := range all {
				_, _ = sp.Write([]byte{b})
			}
			Expect(sp.Finish()).To(Equal(provider.Usage{InputTokens: 7, OutputTokens: 3}))
		})

		It("reads a ConverseStream's metadata event and ignores exceptions", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Event("contentBlockDelta", []byte(`{"contentBlockIndex":0,"delta":{"text":"hello"}}`)),
				bedrocktest.Frame(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"},
					[]byte(`{"usage":{"inputTokens":999,"outputTokens":999}}`)),
				bedrocktest.Event("metadata", []byte(`{"usage":{"inputTokens":15,"outputTokens":6,"totalTokens":21},"metrics":{"latencyMs":100}}`)),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 15, OutputTokens: 6}))
		})

		It("unwraps a native Nova InvokeModel stream's metadata usage", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"messageStart":{"role":"assistant"}}`),
				bedrocktest.Chunk(`{"contentBlockDelta":{"delta":{"text":"hello"},"contentBlockIndex":0}}`),
				bedrocktest.Chunk(`{"metadata":{"usage":{"inputTokens":14,"outputTokens":5}}}`),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 14, OutputTokens: 5}))
		})

		It("keeps invocation metrics ahead of a native Nova stream's own counts", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"metadata":{"usage":{"inputTokens":14,"outputTokens":5}},"amazon-bedrock-invocationMetrics":{"inputTokenCount":15,"outputTokenCount":6}}`),
				bedrocktest.Chunk(`{"metadata":{"usage":{"inputTokens":99,"outputTokens":99}}}`),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 15, OutputTokens: 6}))
		})

		It("approximates a native Nova stream from its text when it carries no counts", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"contentBlockDelta":{"delta":{"text":"abcdefgh"},"contentBlockIndex":0}}`),
			)
			Expect(u).To(Equal(provider.Usage{OutputTokens: 2, Approximated: true}))
		})

		It("reads a Titan stream's running counts", func() {
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"),
				bedrocktest.Chunk(`{"outputText":"hi","index":0,"totalOutputTextTokenCount":2,"inputTextTokenCount":9}`),
				bedrocktest.Chunk(`{"outputText":" there","index":0,"totalOutputTextTokenCount":4,"completionReason":"FINISH"}`),
			)
			Expect(u).To(Equal(provider.Usage{InputTokens: 9, OutputTokens: 4}))
		})

		It("stops at a corrupt frame and approximates from what it decoded", func() {
			good := bedrocktest.Chunk(`{"type":"content_block_delta","delta":{"text":"abcdefgh"}}`)
			bad := bedrocktest.Chunk(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":1,"outputTokenCount":1}}`)
			bad[len(bad)-1] ^= 0xff
			u := feed(p.NewStreamParser("application/vnd.amazon.eventstream"), good, bad)
			Expect(u).To(Equal(provider.Usage{OutputTokens: 2, Approximated: true}))
		})

		DescribeTable("non-stream bodies",
			func(body string, want provider.Usage) {
				Expect(feed(p.NewStreamParser("application/json"), []byte(body))).To(Equal(want))
			},
			Entry("InvokeModel, Claude", `{"type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":12,"output_tokens":4}}`,
				provider.Usage{InputTokens: 12, OutputTokens: 4}),
			Entry("Converse", `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"usage":{"inputTokens":8,"outputTokens":2,"totalTokens":10}}`,
				provider.Usage{InputTokens: 8, OutputTokens: 2}),
			Entry("InvokeModel, Llama", `{"generation":"hi","prompt_token_count":11,"generation_token_count":3}`,
				provider.Usage{InputTokens: 11, OutputTokens: 3}),
			Entry("InvokeModel, Titan", `{"inputTextTokenCount":9,"results":[{"tokenCount":4,"outputText":"hi","completionReason":"FINISH"},{"tokenCount":3,"outputText":"yo"}]}`,
				provider.Usage{InputTokens: 9, OutputTokens: 7}),
			Entry("InvokeModel, Claude Text Completions", `{"completion":"abcdefgh","stop_reason":"stop_sequence"}`,
				provider.Usage{OutputTokens: 2, Approximated: true}),
			Entry("unrecognized", `{"answer":"abcdefgh"}`, provider.Usage{Approximated: true}),
		)
	})

	It("refuses to rewrite a signed request, returning the body untouched", func() {
		body := []byte(claudeBody)
		out, err := p.RewriteModel(signed("/model/"+claudeID+"/invoke", claudeBody), body, "us.anthropic.claude-haiku-4-5")
		Expect(errors.Is(err, bedrock.ErrRewriteUnsupported)).To(BeTrue())
		Expect(out).To(Equal(body))
	})

	It("prices Claude like Anthropic and other families by name", func() {
		Expect(p.Price(claudeID, 1_000_000, 1_000_000)).To(BeNumerically("~", 18, 1e-9))
		Expect(p.Price("amazon.nova-micro-v1:0", 1_000_000, 1_000_000)).To(BeNumerically("~", 0.175, 1e-9))
		Expect(p.Price("meta.llama3-70b-instruct-v1:0", 1_000_000, 0)).To(BeNumerically("~", 3, 1e-9))
	})

	DescribeTable("renders AWS JSON errors SDKs can classify",
		func(status int, typ string) {
			ct, body := p.ErrorResponse(status, "monthly budget reached")
			Expect(ct).To(Equal("application/json"))
			var got map[string]string
			Expect(json.Unmarshal(body, &got)).To(Succeed())
			Expect(got).To(Equal(map[string]string{"__type": typ, "message": "monthly budget reached"}))
		},
		Entry("budget block", http.StatusPaymentRequired, "AccessDeniedException"),
		Entry("rate limit", http.StatusTooManyRequests, "ThrottlingException"),
		Entry("bad gateway", http.StatusBadGateway, "InternalServerException"),
	)
})
