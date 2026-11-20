package cli

// End-to-end specs for the composed serve path (Gate -> enforcement -> metering
// proxy -> SQLite ledger), using the real config-built registry and providers.
// The only fake is the upstream: a recording RoundTripper captures where the
// proxy tried to send each request, then lands it on a local httptest server,
// so no network or vendor credentials are needed.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/enforce"
	"github.com/Fraser-Isbester/inferenced/internal/identity"
	"github.com/Fraser-Isbester/inferenced/internal/pricing"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// upstreamCall is what the proxy attempted to send upstream, captured before
// the test transport redirects it.
type upstreamCall struct {
	URL    string // scheme://host/path?query as the proxy built it
	Host   string
	Header http.Header
	Body   string
}

// recordingTransport records each outbound request and then dials target.
type recordingTransport struct {
	mu     sync.Mutex
	calls  []upstreamCall
	target *url.URL
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	t.mu.Lock()
	t.calls = append(t.calls, upstreamCall{
		URL:    req.URL.String(),
		Host:   req.Host,
		Header: req.Header.Clone(),
		Body:   string(body),
	})
	t.mu.Unlock()

	out := req.Clone(req.Context())
	out.URL.Scheme = t.target.Scheme
	out.URL.Host = t.target.Host
	out.Host = t.target.Host
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	return http.DefaultTransport.RoundTrip(out)
}

func (t *recordingTransport) snapshot() []upstreamCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]upstreamCall(nil), t.calls...)
}

// unitSheet prices every metered provider at $1 per input token and $2 per
// output token, so cost assertions are exact integers. Ollama is deliberately
// absent: it must fall back to its built-in $0 price.
func unitSheet() *pricing.Resolver {
	p := pricing.Price{InputPer1M: 1e6, OutputPer1M: 2e6}
	return pricing.NewResolver(pricing.NewSheet(map[string]map[string]pricing.Price{
		"anthropic": {"default": p},
		"openai":    {"default": p},
		"gemini":    {"default": p},
		"groq":      {"default": p},
		"vertex":    {"default": p},
	}))
}

type gatewayHarness struct {
	gw        *gateway
	srv       *httptest.Server
	st        *store.Store
	dbPath    string
	transport *recordingTransport
	// respond is the fake upstream's behavior; specs set it before sending.
	respond http.HandlerFunc
}

func newGatewayHarness(cfg *config.Config) *gatewayHarness {
	h := &gatewayHarness{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.respond(w, r)
	}))
	DeferCleanup(upstream.Close)
	upURL, err := url.Parse(upstream.URL)
	Expect(err).NotTo(HaveOccurred())

	h.dbPath = filepath.Join(GinkgoT().TempDir(), "ledger.db")
	h.st, err = store.Open(h.dbPath)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(h.st.Close)

	h.gw, err = newGateway(cfg, telemetry.MultiSink(h.st), h.st.Spend, unitSheet())
	Expect(err).NotTo(HaveOccurred())
	h.transport = &recordingTransport{target: upURL}
	h.gw.proxy.Transport = h.transport

	h.srv = httptest.NewServer(h.gw.handler)
	DeferCleanup(h.srv.Close)
	return h
}

// send issues a request to the gateway with an optional Host override and
// returns the status and full body (reading to EOF triggers the ledger emit).
func (h *gatewayHarness) send(method, path, host string, headers map[string]string, body string) (int, http.Header, string) {
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	Expect(err).NotTo(HaveOccurred())
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode, resp.Header, string(got)
}

// rows waits until the ledger holds exactly n events and returns them.
func (h *gatewayHarness) rows(n int) []store.EventRow {
	var got []store.EventRow
	Eventually(func() int {
		var err error
		got, _, err = h.st.RecentEvents(100)
		Expect(err).NotTo(HaveOccurred())
		return len(got)
	}).Should(Equal(n))
	// Hold briefly to catch a duplicate emit landing late.
	Consistently(func() int {
		r, _, _ := h.st.RecentEvents(100)
		return len(r)
	}, 100*time.Millisecond, 20*time.Millisecond).Should(Equal(n))
	return got
}

// ledgerBytes returns the raw SQLite main + WAL file contents, so specs can
// assert a secret never reached disk in any column or page.
func (h *gatewayHarness) ledgerBytes() []byte {
	var all []byte
	for _, p := range []string{h.dbPath, h.dbPath + "-wal"} {
		b, err := os.ReadFile(p)
		if err == nil {
			all = append(all, b...)
		}
	}
	return all
}

const (
	anthropicSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	openaiSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	openaiResponsesJSON = `{"id":"resp_1","object":"response","output":[],` +
		`"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`

	geminiSSE = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"!\"}]}}],\"usageMetadata\":{\"promptTokenCount\":6,\"candidatesTokenCount\":2,\"totalTokenCount\":8}}\n\n"

	ollamaNDJSON = `{"model":"llama3","message":{"content":"Hi"},"done":false}` + "\n" +
		`{"model":"llama3","done":true,"prompt_eval_count":12,"eval_count":9}` + "\n"

	compatJSON = `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`
)

type providerCase struct {
	method, path, host string
	headers            map[string]string
	reqBody            string
	respType, respBody string

	wantURL                 string
	wantProvider, wantModel string
	wantIn, wantOut         int
	wantCost                float64
	credential              string
}

var _ = Describe("gateway (serve wiring) end-to-end", func() {
	var cfg *config.Config

	BeforeEach(func() {
		cfg = config.Default()
		cfg.Providers.OpenAICompatible = []config.CompatEntry{{Name: "groq", Host: "api.groq.com", BasePath: "/openai/v1"}}
	})

	DescribeTable("forwards, streams back unchanged, and meters each provider into the ledger",
		func(c providerCase) {
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.respType)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, c.respBody)
			}

			status, hdr, body := h.send(c.method, c.path, c.host, c.headers, c.reqBody)
			Expect(status).To(Equal(http.StatusOK))
			Expect(hdr.Get("Content-Type")).To(Equal(c.respType),
				"Content-Type must be forwarded; SSE/NDJSON clients dispatch on it")
			Expect(body).To(Equal(c.respBody), "client must see upstream bytes verbatim")

			calls := h.transport.snapshot()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].URL).To(Equal(c.wantURL))
			u, _ := url.Parse(c.wantURL)
			Expect(calls[0].Host).To(Equal(u.Host), "upstream must see its own Host header")
			Expect(calls[0].Body).To(Equal(c.reqBody), "request body must be forwarded byte-identical")
			for k, v := range c.headers {
				Expect(calls[0].Header.Get(k)).To(Equal(v), "header %s must be forwarded", k)
			}

			rows := h.rows(1)
			r := rows[0]
			Expect(r.Provider).To(Equal(c.wantProvider))
			Expect(r.Model).To(Equal(c.wantModel))
			Expect(r.Status).To(Equal(http.StatusOK))
			Expect(r.InputTokens).To(Equal(c.wantIn))
			Expect(r.OutputTokens).To(Equal(c.wantOut))
			Expect(r.CostUSD).To(BeNumerically("~", c.wantCost, 1e-9))
			Expect(r.Team).To(Equal("unknown"), "unattributed traffic is bucketed under the default team")
			Expect(r.DeveloperID).To(Equal(identity.DeveloperID(c.credential)))
			Expect(r.DeveloperID).To(MatchRegexp(`^[0-9a-f]{12}$`))

			Expect(bytes.Contains(h.ledgerBytes(), []byte(c.credential))).To(BeFalse(),
				"raw credential must never be persisted to the ledger")
		},
		Entry("anthropic messages (SSE)", providerCase{
			method: http.MethodPost, path: "/v1/messages",
			headers:  map[string]string{"x-api-key": "sk-ant-e2e-secret", "anthropic-version": "2023-06-01", "Content-Type": "application/json"},
			reqBody:  `{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			respType: "text/event-stream", respBody: anthropicSSE,
			wantURL:      "https://api.anthropic.com/v1/messages",
			wantProvider: "anthropic", wantModel: "claude-sonnet-4-5",
			wantIn: 10, wantOut: 5, wantCost: 10 + 2*5,
			credential: "sk-ant-e2e-secret",
		}),
		Entry("openai chat completions (SSE with usage)", providerCase{
			method: http.MethodPost, path: "/v1/chat/completions",
			headers:  map[string]string{"Authorization": "Bearer sk-openai-e2e-secret", "Content-Type": "application/json"},
			reqBody:  `{"model":"gpt-4o","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
			respType: "text/event-stream", respBody: openaiSSE,
			wantURL:      "https://api.openai.com/v1/chat/completions",
			wantProvider: "openai", wantModel: "gpt-4o",
			wantIn: 11, wantOut: 7, wantCost: 11 + 2*7,
			credential: "sk-openai-e2e-secret",
		}),
		Entry("openai responses (buffered JSON)", providerCase{
			method: http.MethodPost, path: "/v1/responses",
			headers:  map[string]string{"Authorization": "Bearer sk-openai-resp-secret", "Content-Type": "application/json"},
			reqBody:  `{"model":"gpt-4o-mini","input":"hi"}`,
			respType: "application/json", respBody: openaiResponsesJSON,
			wantURL:      "https://api.openai.com/v1/responses",
			wantProvider: "openai", wantModel: "gpt-4o-mini",
			wantIn: 4, wantOut: 3, wantCost: 4 + 2*3,
			credential: "sk-openai-resp-secret",
		}),
		Entry("gemini streamGenerateContent (model in path, key in query)", providerCase{
			method: http.MethodPost, path: "/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse&key=AIza-e2e-secret",
			headers:  map[string]string{"Content-Type": "application/json"},
			reqBody:  `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			respType: "text/event-stream", respBody: geminiSSE,
			wantURL:      "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse&key=AIza-e2e-secret",
			wantProvider: "gemini", wantModel: "gemini-2.0-flash",
			wantIn: 6, wantOut: 2, wantCost: 6 + 2*2,
			credential: "AIza-e2e-secret",
		}),
		Entry("vertex gemini (region routed from the path)", providerCase{
			method: http.MethodPost, path: "/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-2.0-flash:streamGenerateContent?alt=sse",
			headers:  map[string]string{"Authorization": "Bearer ya29.vertex-e2e-secret", "Content-Type": "application/json"},
			reqBody:  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			respType: "text/event-stream", respBody: geminiSSE,
			wantURL:      "https://us-central1-aiplatform.googleapis.com/v1/projects/p1/locations/us-central1/publishers/google/models/gemini-2.0-flash:streamGenerateContent?alt=sse",
			wantProvider: "vertex", wantModel: "gemini-2.0-flash",
			wantIn: 6, wantOut: 2, wantCost: 6 + 2*2,
			credential: "ya29.vertex-e2e-secret",
		}),
		Entry("ollama chat (NDJSON over plain HTTP, $0)", providerCase{
			method: http.MethodPost, path: "/api/chat",
			headers:  map[string]string{"Authorization": "Bearer ollama-e2e-secret", "Content-Type": "application/json"},
			reqBody:  `{"model":"llama3","messages":[{"role":"user","content":"hi"}]}`,
			respType: "application/x-ndjson", respBody: ollamaNDJSON,
			wantURL:      "http://localhost:11434/api/chat",
			wantProvider: "ollama", wantModel: "llama3",
			wantIn: 12, wantOut: 9, wantCost: 0,
			credential: "ollama-e2e-secret",
		}),
		Entry("openai_compatible endpoint selected by Host", providerCase{
			method: http.MethodPost, path: "/openai/v1/chat/completions", host: "api.groq.com",
			headers:  map[string]string{"Authorization": "Bearer gsk-e2e-secret", "Content-Type": "application/json"},
			reqBody:  `{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`,
			respType: "application/json", respBody: compatJSON,
			wantURL:      "https://api.groq.com/openai/v1/chat/completions",
			wantProvider: "groq", wantModel: "llama-3.1-70b",
			wantIn: 3, wantOut: 1, wantCost: 3 + 2*1,
			credential: "gsk-e2e-secret",
		}),
	)

	It("rejects undetected traffic with a 404 without dialing upstream or writing the ledger", func() {
		// Enforcement is live and every caller is over budget, so a 402 here
		// would mean Gate no longer runs outermost.
		cfg.Enforcement = config.Enforcement{
			Enabled: true,
			Limits:  []config.LimitYAML{{Scope: "developer", ID: "*", Period: "day", LimitUSD: 1}},
		}
		h := newGatewayHarness(cfg)
		h.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
		h.st.Emit(telemetry.Event{RequestID: "seed", TimestampUnixMs: time.Now().UnixMilli(), Status: 200, CostUSD: 5})
		h.rows(1)

		status, _, body := h.send(http.MethodGet, "/healthz", "", nil, "")
		Expect(status).To(Equal(http.StatusNotFound))
		Expect(body).To(ContainSubstring("no provider matched"))
		Expect(h.transport.snapshot()).To(BeEmpty())
		Expect(h.rows(1)).To(HaveLen(1), "only the seed row; the rejected request is not metered")
	})

	It("does not route traffic to a provider disabled in config", func() {
		f := false
		cfg.Providers.Anthropic.Enabled = &f
		h := newGatewayHarness(cfg)
		h.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

		status, _, _ := h.send(http.MethodPost, "/v1/messages", "",
			map[string]string{"anthropic-version": "2023-06-01"}, `{"model":"claude-sonnet-4-5"}`)
		Expect(status).To(Equal(http.StatusNotFound))
		Expect(h.transport.snapshot()).To(BeEmpty())
	})

	It("attributes events to the configured developer id and team", func() {
		cfg.Developer = config.Developer{ID: "alice@example.com", Team: "platform"}
		h := newGatewayHarness(cfg)
		h.respond = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, anthropicSSE)
		}
		h.send(http.MethodPost, "/v1/messages", "",
			map[string]string{"x-api-key": "k", "anthropic-version": "2023-06-01"}, `{"model":"claude-sonnet-4-5"}`)
		r := h.rows(1)[0]
		Expect(r.DeveloperID).To(Equal("alice@example.com"))
		Expect(r.Team).To(Equal("platform"))
	})

	It("records an upstream transport failure as a 502 at $0", func() {
		h := newGatewayHarness(cfg)
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL, _ := url.Parse(dead.URL)
		dead.Close() // connection refused from here on
		h.transport.target = deadURL

		status, _, _ := h.send(http.MethodPost, "/v1/messages", "",
			map[string]string{"anthropic-version": "2023-06-01"}, `{"model":"claude-sonnet-4-5"}`)
		Expect(status).To(Equal(http.StatusBadGateway))
		r := h.rows(1)[0]
		Expect(r.Status).To(Equal(http.StatusBadGateway))
		Expect(r.CostUSD).To(BeZero())
	})

	Context("with spend enforcement backed by the ledger", func() {
		anthReq := func(h *gatewayHarness, model string) (int, http.Header, string) {
			return h.send(http.MethodPost, "/v1/messages", "",
				map[string]string{"x-api-key": "sk-ant-dev", "anthropic-version": "2023-06-01"},
				`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		}

		BeforeEach(func() {
			cfg.Enforcement = config.Enforcement{
				Enabled: true,
				Limits:  []config.LimitYAML{{Scope: "developer", ID: "*", Period: "day", LimitUSD: 15}},
			}
		})

		It("blocks once recorded spend reaches the limit, with a provider-native 402", func() {
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicSSE) // costs $20 under unitSheet
			}

			status, _, _ := anthReq(h, "claude-sonnet-4-5")
			Expect(status).To(Equal(http.StatusOK))
			h.rows(1) // spend is visible only after the stream completes

			status, hdr, body := anthReq(h, "claude-sonnet-4-5")
			Expect(status).To(Equal(http.StatusPaymentRequired))
			Expect(hdr.Get("Content-Type")).To(ContainSubstring("application/json"))
			Expect(body).To(ContainSubstring(`"type":"error"`), "Anthropic clients need their own error envelope")
			Expect(body).To(ContainSubstring("exceeded"))

			Expect(h.transport.snapshot()).To(HaveLen(1), "a blocked request must never be forwarded")
			Expect(h.rows(1)).To(HaveLen(1), "blocked requests are not metered")
		})

		It("does not count non-2xx upstream responses toward spend", func() {
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, anthropicSSE)
			}
			for range 3 {
				status, _, _ := anthReq(h, "claude-sonnet-4-5")
				Expect(status).To(Equal(http.StatusTooManyRequests))
			}
			rows := h.rows(3)
			for _, r := range rows {
				Expect(r.CostUSD).To(BeZero())
			}
			Expect(h.transport.snapshot()).To(HaveLen(3), "retries after 429 must not be blocked")
		})

		It("downgrades the model between the downgrade and block thresholds and meters the downgrade", func() {
			cfg.Enforcement.Limits[0].DowngradeAt = 0.5
			cfg.Enforcement.Limits[0].BlockAt = 2
			cfg.Enforcement.Downgrade = []config.DowngradeRuleYAML{{Match: "opus", To: "claude-haiku-4-5"}}
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicSSE)
			}
			// Seed $10 of prior spend: >= 50% of $15, < 200%.
			h.st.Emit(telemetry.Event{
				RequestID: "seed", TimestampUnixMs: time.Now().UnixMilli(),
				Provider: "anthropic", DeveloperID: identity.DeveloperID("sk-ant-dev"),
				Status: 200, CostUSD: 10,
			})

			status, _, _ := anthReq(h, "claude-opus-4-5")
			Expect(status).To(Equal(http.StatusOK))

			calls := h.transport.snapshot()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].Body).To(ContainSubstring(`"model":"claude-haiku-4-5"`))
			Expect(calls[0].Body).To(ContainSubstring(`"stream":true`), "other fields survive the rewrite")
			Expect(calls[0].Body).NotTo(ContainSubstring("opus"))

			rows := h.rows(2)
			Expect(rows[1].Model).To(Equal("claude-haiku-4-5"), "the ledger records the model actually served")
		})

		It("forwards everything in warn mode even when over the limit", func() {
			cfg.Enforcement.Action = "warn"
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicSSE)
			}
			for range 2 {
				status, _, _ := anthReq(h, "claude-sonnet-4-5")
				Expect(status).To(Equal(http.StatusOK))
				h.rows(len(h.transport.snapshot()))
			}
			Expect(h.transport.snapshot()).To(HaveLen(2))
		})

		It("applies a hot-swapped ruleset to the very next request", func() {
			h := newGatewayHarness(cfg)
			h.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, anthropicSSE)
			}
			anthReq(h, "claude-sonnet-4-5")
			h.rows(1)

			// Simulate the config watcher disabling enforcement.
			off := config.Default()
			rs, err := buildRuleset(off, h.st.Spend)
			Expect(err).NotTo(HaveOccurred())
			h.gw.rules.Store(&rs)

			status, _, _ := anthReq(h, "claude-sonnet-4-5")
			Expect(status).To(Equal(http.StatusOK))

			// And re-enabling it blocks again.
			on, err := buildRuleset(cfg, h.st.Spend)
			Expect(err).NotTo(HaveOccurred())
			h.gw.rules.Store(&on)
			h.rows(2)
			status, _, _ = anthReq(h, "claude-sonnet-4-5")
			Expect(status).To(Equal(http.StatusPaymentRequired))
		})
	})
})

// Compile-time guard that the gateway's ruleset pointer holds the type the
// watcher swaps in.
var _ = func(g *gateway) *enforce.Ruleset { return g.rules.Load() }
