package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// cannedStream is a minimal but well-formed Anthropic Messages SSE response:
// message_start (input_tokens=10), a content_block_delta, message_delta
// (output_tokens=5), and message_stop.
const cannedStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// capturingSink records the Events emitted by the proxy so specs can assert on
// the metered usage and cost.
type capturingSink struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (s *capturingSink) Emit(e telemetry.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *capturingSink) last() (telemetry.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return telemetry.Event{}, false
	}
	return s.events[len(s.events)-1], true
}

// redirectTransport dials a fixed upstream address regardless of the host the
// proxy Director rewrote the request to, keeping the spec fully hermetic: the
// proxy believes it is talking to api.anthropic.com over HTTPS, but every
// connection actually lands on the local httptest server.
type redirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = t.target.Scheme
	req.URL.Host = t.target.Host
	req.Host = t.target.Host
	return t.base.RoundTrip(req)
}

var _ = Describe("Proxy end-to-end", func() {
	It("tees an Anthropic stream byte-for-byte while metering usage and cost", func() {
		// Fake upstream returns the canned SSE stream.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/v1/messages"))
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, err := io.WriteString(w, cannedStream)
			Expect(err).NotTo(HaveOccurred())
		}))
		defer upstream.Close()

		upURL, err := url.Parse(upstream.URL)
		Expect(err).NotTo(HaveOccurred())

		reg := provider.NewRegistry()
		reg.Register(anthropic.New())

		sink := &capturingSink{}
		rp := proxy.New(reg, sink)

		// Override Transport so every upstream dial hits the httptest server,
		// regardless of the host the Director rewrote (api.anthropic.com).
		rp.Transport = &redirectTransport{target: upURL, base: http.DefaultTransport}

		// Front the proxy with a local server so we exercise the full net stack.
		proxySrv := httptest.NewServer(rp)
		defer proxySrv.Close()

		reqBody := `{"model":"claude-3-5-sonnet","stream":true,` +
			`"messages":[{"role":"user","content":"Hi"}]}`
		req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
			strings.NewReader(reqBody))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "sk-ant-test-secret")
		req.Header.Set("anthropic-version", "2023-06-01")

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		// Read the FULL body first: the Event is emitted only after body EOF.
		got, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())

		// (a) Client-visible bytes must be byte-identical to what upstream sent.
		Expect(string(got)).To(Equal(cannedStream))

		// (b) The captured Event must reflect metered usage and a positive cost.
		// The emit goroutine finishes shortly after body EOF.
		Eventually(func() bool { _, ok := sink.last(); return ok }).Should(BeTrue())

		ev, _ := sink.last()
		Expect(ev.Provider).To(Equal("anthropic"))
		Expect(ev.InputTokens).To(Equal(10))
		Expect(ev.OutputTokens).To(Equal(5))
		Expect(ev.CostUSD).To(BeNumerically(">", 0))
		Expect(ev.TokensApproximated).To(BeFalse())
	})
})
