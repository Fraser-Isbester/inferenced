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
	"github.com/Fraser-Isbester/inferenced/internal/provider/vertex"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
)

// hostRecorder notes the host the proxy meant to dial before redirecting the
// connection to the local fake upstream.
type hostRecorder struct {
	redirectTransport
	mu   sync.Mutex
	host string
}

func (t *hostRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.host = req.URL.Host
	t.mu.Unlock()
	return t.redirectTransport.RoundTrip(req)
}

var _ = Describe("Proxy with Vertex AI", func() {
	It("routes Claude-on-Vertex to the path's region and meters it with the Anthropic parser", func() {
		const path = "/v1/projects/p1/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict"
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal(path))
			Expect(r.Header.Get("Authorization")).To(Equal("Bearer ya29.adc-token"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, cannedStream)
		}))
		defer upstream.Close()
		upURL, err := url.Parse(upstream.URL)
		Expect(err).NotTo(HaveOccurred())

		// Same order as config.Registry: vertex ahead of anthropic, which
		// would otherwise claim the request via its anthropic-version header.
		reg := provider.NewRegistry()
		reg.Register(vertex.New())
		reg.Register(anthropic.New())

		sink := &capturingSink{}
		rp := proxy.New(reg, sink, proxy.Options{})
		tr := &hostRecorder{redirectTransport: redirectTransport{target: upURL, base: http.DefaultTransport}}
		rp.Transport = tr
		srv := httptest.NewServer(rp)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost, srv.URL+path,
			strings.NewReader(`{"anthropic_version":"vertex-2023-10-16","stream":true,"messages":[{"role":"user","content":"Hi"}]}`))
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Authorization", "Bearer ya29.adc-token")
		req.Header.Set("anthropic-version", "vertex-2023-10-16")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(got)).To(Equal(cannedStream))

		tr.mu.Lock()
		Expect(tr.host).To(Equal("us-east5-aiplatform.googleapis.com"))
		tr.mu.Unlock()

		Eventually(func() bool { _, ok := sink.last(); return ok }).Should(BeTrue())
		ev, _ := sink.last()
		Expect(ev.Provider).To(Equal("vertex"))
		Expect(ev.Model).To(Equal("claude-sonnet-4-5@20250929"))
		Expect(ev.InputTokens).To(Equal(10))
		Expect(ev.OutputTokens).To(Equal(5))
		Expect(ev.TokensApproximated).To(BeFalse())
		Expect(ev.CostUSD).To(BeNumerically(">", 0))
	})
})
