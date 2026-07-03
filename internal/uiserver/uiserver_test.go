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
		st      *store.Store
		srv     *httptest.Server
		cfgPath string
	)

	BeforeEach(func() {
		dir := GinkgoT().TempDir()
		var err error
		st, err = store.Open(filepath.Join(dir, "ui.db"))
		Expect(err).NotTo(HaveOccurred())
		st.Emit(telemetry.Event{
			RequestID: "1", TimestampUnixMs: time.Now().UnixMilli(),
			Provider: "anthropic", Model: "claude-sonnet-4-5", DeveloperID: "dev-1",
			Team: "platform", Status: 200, InputTokens: 10, OutputTokens: 5, CostUSD: 1.5,
			Metadata: `{"files":["main.go"],"commands":["git status"]}`,
		})

		// A config with two limits so the gauge/rules endpoints have data.
		cfgPath = filepath.Join(dir, "inferenced.yaml")
		cfg := config.Default()
		cfg.Enforcement = config.Enforcement{
			Enabled: true,
			Limits: []config.LimitYAML{
				{Scope: "team", ID: "platform", Period: "day", LimitUSD: 5},
				{Scope: "developer", ID: "*", Period: "day", LimitUSD: 10, DowngradeAt: 0.8, BlockAt: 1.2},
			},
		}
		Expect(cfg.Save(cfgPath)).To(Succeed())

		srv = httptest.NewServer(uiserver.Handler(st, cfgPath))
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

	It("returns a request's metadata blob at /api/event", func() {
		resp, err := http.Get(srv.URL + "/api/event?row_id=1")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var out struct {
			Metadata struct {
				Files    []string `json:"files"`
				Commands []string `json:"commands"`
			} `json:"metadata"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Metadata.Files).To(ConsistOf("main.go"))
		Expect(out.Metadata.Commands).To(ConsistOf("git status"))
	})

	It("400s a bad row_id at /api/event", func() {
		resp, err := http.Get(srv.URL + "/api/event?row_id=abc")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))
	})

	It("returns spend-vs-limit gauges at /api/limits", func() {
		resp, err := http.Get(srv.URL + "/api/limits")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var gauges []map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&gauges)).To(Succeed())
		Expect(gauges).To(HaveLen(2))
		// team/platform limit $5, spent $1.5 by the seeded event, blocks at $5.
		Expect(gauges[0]).To(HaveKeyWithValue("id", "platform"))
		Expect(gauges[0]).To(HaveKeyWithValue("spent_usd", 1.5))
		Expect(gauges[0]).To(HaveKeyWithValue("block_threshold_usd", 5.0))
		// developer/* soft limit: downgrade at $8, block at $12.
		Expect(gauges[1]).To(HaveKeyWithValue("downgrade_threshold_usd", 8.0))
		Expect(gauges[1]).To(HaveKeyWithValue("block_threshold_usd", 12.0))
	})

	It("round-trips edited rules through PUT /api/rules", func() {
		body := `{"enabled":true,"action":"block","limits":[{"scope":"team","id":"platform","period":"day","limit_usd":50}]}`
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/rules", strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

		// The saved file must reflect the edit.
		saved, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(saved.Enforcement.Limits).To(HaveLen(1))
		Expect(saved.Enforcement.Limits[0].LimitUSD).To(Equal(50.0))
	})

	It("rejects an invalid rules edit with 400 and leaves the file untouched", func() {
		before, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred())

		// downgrade_at >= block_at is invalid.
		body := `{"enabled":true,"limits":[{"scope":"team","id":"x","period":"day","limit_usd":10,"downgrade_at":1.5,"block_at":1.0}]}`
		req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/rules", strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusBadRequest))

		after, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.Enforcement.Limits).To(HaveLen(len(before.Enforcement.Limits)))
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
