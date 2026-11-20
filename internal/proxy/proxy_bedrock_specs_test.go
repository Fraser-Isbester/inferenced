package proxy_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/bedrock"
	"github.com/Fraser-Isbester/inferenced/internal/provider/bedrock/bedrocktest"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
)

// signedHostRecorder also notes the Host header the proxy set, which SigV4
// signs, before the redirect overwrites it.
type signedHostRecorder struct {
	hostRecorder
	hostHeader string
}

func (t *signedHostRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.hostHeader = req.Host
	t.mu.Unlock()
	return t.hostRecorder.RoundTrip(req)
}

var _ = Describe("Proxy with AWS Bedrock (transparent interception)", func() {
	const (
		bedrockHost = "bedrock-runtime.us-west-2.amazonaws.com"
		path        = "/model/us.anthropic.claude-sonnet-4-5-20250929-v1:0/invoke-with-response-stream"
		reqBody     = `{"anthropic_version":"bedrock-2023-05-31","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`
		// A SigV4 Authorization that signs accept-encoding: dropping that header
		// in flight would make AWS reject the request.
		auth = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261120/us-west-2/bedrock/aws4_request, " +
			"SignedHeaders=accept-encoding;content-type;host;x-amz-date, Signature=0123abcd"
	)

	var stream []byte
	BeforeEach(func() {
		stream = bytes.Join([][]byte{
			bedrocktest.Chunk(`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`),
			bedrocktest.Chunk(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`),
			bedrocktest.Chunk(`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":10,"outputTokenCount":5}}`),
		}, nil)
	})

	It("forwards the signed request unchanged to the host it was signed for, and meters the event stream", func() {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			b, _ := io.ReadAll(r.Body)
			Expect(string(b)).To(Equal(reqBody))
			Expect(r.URL.Path).To(Equal(path))
			Expect(r.Header.Get("Authorization")).To(Equal(auth))
			Expect(r.Header.Get("X-Amz-Date")).To(Equal("20261120T112233Z"))
			Expect(r.Header.Get("Accept-Encoding")).To(Equal("identity"))
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(stream)
		}))
		defer upstream.Close()
		upURL, err := url.Parse(upstream.URL)
		Expect(err).NotTo(HaveOccurred())

		reg := provider.NewRegistry()
		reg.Register(bedrock.New())
		reg.Register(anthropic.New())

		sink := &capturingSink{}
		rp := proxy.New(reg, sink, proxy.Options{})
		tr := &signedHostRecorder{hostRecorder: hostRecorder{redirectTransport: redirectTransport{target: upURL, base: http.DefaultTransport}}}
		rp.Transport = tr
		srv := httptest.NewServer(rp)
		defer srv.Close()

		req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewBufferString(reqBody))
		Expect(err).NotTo(HaveOccurred())
		req.Host = bedrockHost // what a transparently intercepted client sends
		req.Header.Set("Authorization", auth)
		req.Header.Set("X-Amz-Date", "20261120T112233Z")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(got).To(Equal(stream))

		tr.mu.Lock()
		Expect(tr.host).To(Equal(bedrockHost))
		Expect(tr.hostHeader).To(Equal(bedrockHost), "the signed Host header")
		tr.mu.Unlock()

		Eventually(func() bool { _, ok := sink.last(); return ok }).Should(BeTrue())
		ev, _ := sink.last()
		Expect(ev.Provider).To(Equal("bedrock"))
		Expect(ev.Model).To(Equal("us.anthropic.claude-sonnet-4-5-20250929-v1:0"))
		Expect(ev.InputTokens).To(Equal(10))
		Expect(ev.OutputTokens).To(Equal(5))
		Expect(ev.TokensApproximated).To(BeFalse())
		Expect(ev.CostUSD).To(BeNumerically(">", 0))
	})
})
