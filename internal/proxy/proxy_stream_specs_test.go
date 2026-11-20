package proxy_test

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
)

// Invariants of the tee: the client path is never buffered behind the
// observer, an aborted stream still emits exactly once, and concurrent requests
// never share or lose per-request state.

func (s *capturingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// startProxy fronts a fresh anthropic-only proxy with a local server whose
// upstream dials land on handler.
func startProxy(handler http.HandlerFunc) (*httptest.Server, *capturingSink) {
	upstream := httptest.NewServer(handler)
	DeferCleanup(upstream.Close)
	upURL, err := url.Parse(upstream.URL)
	Expect(err).NotTo(HaveOccurred())

	reg := provider.NewRegistry()
	reg.Register(anthropic.New())
	sink := &capturingSink{}
	rp := proxy.New(reg, sink, proxy.Options{})
	rp.Transport = &redirectTransport{target: upURL, base: http.DefaultTransport}

	srv := httptest.NewServer(rp)
	DeferCleanup(srv.Close)
	return srv, sink
}

func messagesRequest(srvURL string) *http.Request {
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true}`))
	Expect(err).NotTo(HaveOccurred())
	req.Header.Set("anthropic-version", "2023-06-01")
	return req
}

const firstEvent = "event: message_start\n" +
	`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n"

const restOfStream = "event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

var _ = Describe("Proxy streaming invariants", func() {
	It("delivers each SSE event to the client before the upstream stream completes", func() {
		release := make(chan struct{})
		srv, sink := startProxy(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, firstEvent)
			w.(http.Flusher).Flush()
			<-release // hold the stream open until the client has seen event 1
			_, _ = io.WriteString(w, restOfStream)
		})
		defer func() {
			select {
			case <-release:
			default:
				close(release)
			}
		}()

		// The timeout covers Do and every body read: a proxy that buffers before
		// sending headers would otherwise block Do until release, hanging the
		// suite instead of failing it.
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(messagesRequest(srv.URL))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()

		// If the proxy buffered (e.g. FlushInterval regressed, or the observer
		// back-pressured the client path), this read would hang until release.
		br := bufio.NewReader(resp.Body)
		gotFirst := make(chan string, 1)
		go func() {
			var b strings.Builder
			for !strings.HasSuffix(b.String(), "\n\n") {
				line, err := br.ReadString('\n')
				b.WriteString(line)
				if err != nil {
					break
				}
			}
			gotFirst <- b.String()
		}()
		Eventually(gotFirst, 2*time.Second).Should(Receive(Equal(firstEvent)))
		Expect(sink.count()).To(BeZero(), "no event may be emitted mid-stream")

		close(release)
		rest, err := io.ReadAll(br)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(rest)).To(Equal(restOfStream))

		Eventually(sink.count).Should(Equal(1))
		ev, _ := sink.last()
		Expect(ev.InputTokens).To(Equal(10))
		Expect(ev.OutputTokens).To(Equal(5))
	})

	It("emits exactly one event when the client disconnects mid-stream", func() {
		upstreamDone := make(chan struct{})
		cancelled := make(chan struct{})
		// teardown unblocks the handler independently of the behavior under test,
		// so a cancel-propagation regression fails the spec instead of hanging
		// httptest.Server.Close on an active handler.
		teardown := make(chan struct{})
		defer close(teardown)
		srv, sink := startProxy(func(w http.ResponseWriter, r *http.Request) {
			defer close(upstreamDone)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, firstEvent)
			w.(http.Flusher).Flush()
			select { // upstream would stream forever; only a cancel ends it
			case <-r.Context().Done():
				close(cancelled)
			case <-teardown:
			}
		})

		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(messagesRequest(srv.URL))
		Expect(err).NotTo(HaveOccurred())
		buf := make([]byte, len(firstEvent))
		_, err = io.ReadFull(resp.Body, buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Body.Close()).To(Succeed()) // client walks away

		// The cancel must propagate upstream and unblock the observer goroutine.
		Eventually(cancelled, 3*time.Second).Should(BeClosed())
		Eventually(upstreamDone, 3*time.Second).Should(BeClosed())
		Eventually(sink.count, 3*time.Second).Should(Equal(1))
		Consistently(sink.count, 200*time.Millisecond).Should(Equal(1))

		ev, _ := sink.last()
		Expect(ev.InputTokens).To(Equal(10), "usage seen before the abort is kept")
	})

	It("keeps per-request state isolated under concurrency", func() {
		// Each upstream response echoes the request's model and a distinct input
		// token count so cross-request bleed is detectable.
		srv, sink := startProxy(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var n int
			_, _ = fmt.Sscanf(string(body), `{"model":"m-%d"`, &n)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}}\n\n", n)
			_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":%d}}\n\n", n*2)
		})

		const n = 40
		var wg sync.WaitGroup
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer GinkgoRecover()
				defer wg.Done()
				req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
					strings.NewReader(fmt.Sprintf(`{"model":"m-%d","stream":true}`, i)))
				Expect(err).NotTo(HaveOccurred())
				req.Header.Set("anthropic-version", "2023-06-01")
				resp, err := http.DefaultClient.Do(req)
				Expect(err).NotTo(HaveOccurred())
				_, _ = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}(i)
		}
		wg.Wait()

		Eventually(sink.count, 5*time.Second).Should(Equal(n))
		sink.mu.Lock()
		defer sink.mu.Unlock()
		ids := map[string]bool{}
		seen := map[int]bool{}
		for _, ev := range sink.events {
			Expect(ids[ev.RequestID]).To(BeFalse(), "request IDs must be unique")
			ids[ev.RequestID] = true
			var i int
			_, err := fmt.Sscanf(ev.Model, "m-%d", &i)
			Expect(err).NotTo(HaveOccurred())
			Expect(i).To(BeNumerically(">=", 1))
			Expect(i).To(BeNumerically("<=", n))
			Expect(seen[i]).To(BeFalse(), "model %s emitted twice; another request's event was lost", ev.Model)
			seen[i] = true
			Expect(ev.InputTokens).To(Equal(i), "event for %s carries another request's usage", ev.Model)
			Expect(ev.OutputTokens).To(Equal(2 * i))
		}
		Expect(seen).To(HaveLen(n), "every request must be represented exactly once")
	})
})
