package uiserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
	"github.com/Fraser-Isbester/inferenced/internal/uiserver"
)

func TestUIServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "UI Server Suite")
}

var _ = Describe("Handler", func() {
	var (
		st  *store.Store
		srv *httptest.Server
	)

	BeforeEach(func() {
		var err error
		st, err = store.Open(filepath.Join(GinkgoT().TempDir(), "ui.db"))
		Expect(err).NotTo(HaveOccurred())
		st.Emit(telemetry.Event{
			RequestID: "1", TimestampUnixMs: time.Now().UnixMilli(),
			Provider: "anthropic", Model: "claude-sonnet-4-5", DeveloperID: "dev-1",
			Team: "platform", Status: 200, InputTokens: 10, OutputTokens: 5, CostUSD: 1.5,
		})
		srv = httptest.NewServer(uiserver.Handler(st, config.Default()))
		DeferCleanup(func() {
			srv.Close()
			_ = st.Close()
		})
	})

	It("serves the dashboard HTML at /", func() {
		resp, err := http.Get(srv.URL + "/")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/html"))
	})

	It("404s an unknown path", func() {
		resp, err := http.Get(srv.URL + "/nope")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNotFound))
	})

	It("returns the spend report as JSON", func() {
		resp, err := http.Get(srv.URL + "/api/spend?period=day")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var report store.Report
		Expect(json.NewDecoder(resp.Body).Decode(&report)).To(Succeed())
		Expect(report.TotalUSD).To(Equal(1.5))
		Expect(report.TotalRequests).To(Equal(1))
		Expect(report.ByModel).To(HaveLen(1))
		Expect(report.ByModel[0].Key).To(Equal("claude-sonnet-4-5"))
	})

	It("exposes the config read-only as YAML", func() {
		resp, err := http.Get(srv.URL + "/api/config")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("yaml"))
	})

	It("replays recent events over SSE on connect", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream", nil)
		Expect(err).NotTo(HaveOccurred())

		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

		// Read the first SSE data frame from the initial replay.
		scanner := bufio.NewScanner(resp.Body)
		var payload string
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				payload = data
				break
			}
		}
		Expect(payload).NotTo(BeEmpty())
		var row store.EventRow
		Expect(json.Unmarshal([]byte(payload), &row)).To(Succeed())
		Expect(row.Provider).To(Equal("anthropic"))
		Expect(row.CostUSD).To(Equal(1.5))
	})
})
