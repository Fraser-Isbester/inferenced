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

	"github.com/Fraser-Isbester/inferenced/internal/artifact"
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
			Artifacts: []artifact.Artifact{
				{Kind: "pull_request", Source: "github", URL: "https://github.com/a/b/pull/3", Ref: "#3"},
			},
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

	It("returns users/teams with status at /api/overview", func() {
		resp, err := http.Get(srv.URL + "/api/overview")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var ov struct {
			Enforcement struct {
				Enabled bool `json:"enabled"`
			} `json:"enforcement"`
			Subjects []map[string]any `json:"subjects"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&ov)).To(Succeed())
		Expect(ov.Enforcement.Enabled).To(BeTrue())
		// The seeded event is dev-1 on team platform; team platform has a $5/day
		// limit and $1.50 spent → a user row and a team row, team not yet alerting.
		var team map[string]any
		for _, s := range ov.Subjects {
			if s["kind"] == "team" && s["id"] == "platform" {
				team = s
			}
		}
		Expect(team).NotTo(BeNil())
		Expect(team["block_threshold_usd"]).To(Equal(5.0))
		Expect(team["status"]).To(Equal("ok"))
	})

	It("upserts and deletes a single limit via /api/limit", func() {
		body := `{"scope":"developer","id":"alice","period":"day","limit_usd":25,"downgrade_at":0.5,"block_at":1.0}`
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/limit", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

		saved, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(saved.Enforcement.Enabled).To(BeTrue())
		var found *config.LimitYAML
		for i := range saved.Enforcement.Limits {
			if saved.Enforcement.Limits[i].ID == "alice" {
				found = &saved.Enforcement.Limits[i]
			}
		}
		Expect(found).NotTo(BeNil())
		Expect(found.LimitUSD).To(Equal(25.0))

		// Delete it again.
		delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/limit?scope=developer&id=alice&period=day", nil)
		delResp, err := http.DefaultClient.Do(delReq)
		Expect(err).NotTo(HaveOccurred())
		_ = delResp.Body.Close()
		Expect(delResp.StatusCode).To(Equal(http.StatusNoContent))

		after, err := config.Load(cfgPath)
		Expect(err).NotTo(HaveOccurred())
		for _, l := range after.Enforcement.Limits {
			Expect(l.ID).NotTo(Equal("alice"))
		}
	})

	It("returns produced artifacts at /api/artifacts", func() {
		resp, err := http.Get(srv.URL + "/api/artifacts?period=rolling&window=720h")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		var rows []map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&rows)).To(Succeed())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0]).To(HaveKeyWithValue("kind", "pull_request"))
		Expect(rows[0]).To(HaveKeyWithValue("url", "https://github.com/a/b/pull/3"))
	})

	It("includes headline totals in /api/overview", func() {
		resp, err := http.Get(srv.URL + "/api/overview")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		var ov struct {
			Totals struct {
				TodayUSD      float64 `json:"today_usd"`
				TodayRequests int     `json:"today_requests"`
			} `json:"totals"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&ov)).To(Succeed())
		Expect(ov.Totals.TodayUSD).To(Equal(1.5)) // the seeded event
		Expect(ov.Totals.TodayRequests).To(Equal(1))
	})

	It("returns a producing request at /api/request", func() {
		st.Emit(telemetry.Event{
			RequestID: "rq1", TimestampUnixMs: 999000, DeveloperID: "dev-1",
			Provider: "anthropic", Model: "claude-opus-4-8", Status: 200, CostUSD: 0.02,
			Metadata: `{"commands":["gh pr create"]}`,
		})
		resp, err := http.Get(srv.URL + "/api/request?id=rq1&ts=999000")
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		Expect(resp.StatusCode).To(Equal(http.StatusOK))
		var d map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&d)).To(Succeed())
		Expect(d).To(HaveKeyWithValue("model", "claude-opus-4-8"))
		Expect(d).To(HaveKeyWithValue("cost_usd", 0.02))
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
