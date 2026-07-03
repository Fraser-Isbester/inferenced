package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
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

// capturingSink records the single Event emitted by the proxy so the test can
// assert on the metered usage and cost.
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
// proxy Director rewrote the request to. This keeps the test fully hermetic:
// the proxy believes it is talking to api.anthropic.com over HTTPS, but every
// connection actually lands on the local httptest server.
type redirectTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Rewrite scheme/host to the fake upstream while preserving the path.
	req.URL.Scheme = t.target.Scheme
	req.URL.Host = t.target.Host
	req.Host = t.target.Host
	return t.base.RoundTrip(req)
}

// TestProxyEndToEndAnthropicStream stands up a fake Anthropic upstream, routes a
// streaming /v1/messages request through the real proxy, and verifies both that
// the client-visible bytes are byte-identical to the upstream response and that
// the telemetry Event carries the metered token usage and a positive cost.
func TestProxyEndToEndAnthropicStream(t *testing.T) {
	// Fake upstream returns the canned SSE stream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, cannedStream); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	}))
	defer upstream.Close()

	upURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	reg := provider.NewRegistry()
	reg.Register(anthropic.New())

	sink := &capturingSink{}
	rp := New(reg, sink)

	// Override Transport so every upstream dial hits the httptest server,
	// regardless of the host the Director rewrote (api.anthropic.com).
	rp.Transport = &redirectTransport{
		target: upURL,
		base:   http.DefaultTransport,
	}

	// Front the proxy with a local server so we exercise the full net stack.
	proxySrv := httptest.NewServer(rp)
	defer proxySrv.Close()

	reqBody := `{"model":"claude-3-5-sonnet","stream":true,` +
		`"messages":[{"role":"user","content":"Hi"}]}`
	req, err := http.NewRequest(http.MethodPost, proxySrv.URL+"/v1/messages",
		strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-secret")
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read the FULL body first: the Event is emitted only after the body EOFs.
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// (a) Client-visible bytes must be byte-identical to what upstream sent.
	if string(got) != cannedStream {
		t.Errorf("client body mismatch:\n got: %q\nwant: %q", got, cannedStream)
	}

	// (b) The captured Event must reflect metered usage and a positive cost.
	// The emit goroutine finishes shortly after body EOF; poll briefly.
	var ev telemetry.Event
	deadline := time.Now().Add(2 * time.Second)
	for {
		var ok bool
		ev, ok = sink.last()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no telemetry Event emitted within deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if ev.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", ev.Provider)
	}
	if ev.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", ev.InputTokens)
	}
	if ev.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", ev.OutputTokens)
	}
	if ev.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0", ev.CostUSD)
	}
	if ev.TokensApproximated {
		t.Errorf("TokensApproximated = true, want false (authoritative usage)")
	}
}
