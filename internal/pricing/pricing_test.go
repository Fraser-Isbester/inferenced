package pricing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/pricing"
)

var _ = Describe("Sheet.Lookup", func() {
	sheet := pricing.NewSheet(map[string]map[string]pricing.Price{
		"anthropic": {
			"default": {InputPer1M: 3, OutputPer1M: 15},
			"opus":    {InputPer1M: 15, OutputPer1M: 75},
			"haiku":   {InputPer1M: 0.8, OutputPer1M: 4},
		},
		"openai": {
			"gpt-4o":      {InputPer1M: 2.5, OutputPer1M: 10},
			"gpt-4o-mini": {InputPer1M: 0.15, OutputPer1M: 0.6},
		},
	})

	It("matches a family by substring, case-insensitively", func() {
		p, ok := sheet.Lookup("anthropic", "claude-3-OPUS-20240229")
		Expect(ok).To(BeTrue())
		Expect(p.OutputPer1M).To(Equal(75.0))
	})

	It("prefers the longest substring match (gpt-4o-mini over gpt-4o)", func() {
		p, ok := sheet.Lookup("openai", "gpt-4o-mini-2024-07-18")
		Expect(ok).To(BeTrue())
		Expect(p.InputPer1M).To(Equal(0.15))
	})

	It("falls back to the provider default when no matcher hits", func() {
		p, ok := sheet.Lookup("anthropic", "claude-sonnet-4-5")
		Expect(ok).To(BeTrue())
		Expect(p.InputPer1M).To(Equal(3.0))
	})

	It("reports a miss when the provider has no default and nothing matches", func() {
		_, ok := sheet.Lookup("openai", "text-embedding-3-small")
		Expect(ok).To(BeFalse())
	})

	It("reports a miss for an unknown provider", func() {
		_, ok := sheet.Lookup("cohere", "command-r")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("Sheet.Cost", func() {
	It("computes input+output cost per million tokens", func() {
		sheet := pricing.NewSheet(map[string]map[string]pricing.Price{
			"anthropic": {"sonnet": {InputPer1M: 3, OutputPer1M: 15}},
		})
		cost, ok := sheet.Cost("anthropic", "claude-sonnet-4-5", 1_000_000, 2_000_000)
		Expect(ok).To(BeTrue())
		Expect(cost).To(BeNumerically("~", 3+30, 1e-9))
	})
})

var _ = Describe("Default", func() {
	It("loads the embedded sheet and prices the built-in families", func() {
		sheet := pricing.Default()
		Expect(sheet.Empty()).To(BeFalse())

		p, ok := sheet.Lookup("anthropic", "claude-opus-4-5")
		Expect(ok).To(BeTrue())
		Expect(p.InputPer1M).To(Equal(15.0))

		_, ok = sheet.Lookup("gemini", "gemini-1.5-flash")
		Expect(ok).To(BeTrue())
	})
})

var _ = Describe("Resolver", func() {
	It("consults sheets in priority order and falls back on miss", func() {
		high := pricing.NewSheet(map[string]map[string]pricing.Price{
			"anthropic": {"opus": {InputPer1M: 99, OutputPer1M: 99}},
		})
		low := pricing.Default()
		r := pricing.NewResolver(high, low)

		// High-priority sheet wins for opus.
		cost, ok := r.Price("anthropic", "claude-opus-4-5", 1_000_000, 0)
		Expect(ok).To(BeTrue())
		Expect(cost).To(BeNumerically("~", 99, 1e-9))

		// Falls through to the embedded default for haiku (not in the high sheet).
		cost, ok = r.Price("anthropic", "claude-haiku-4-5", 1_000_000, 0)
		Expect(ok).To(BeTrue())
		Expect(cost).To(BeNumerically("~", 0.8, 1e-9))
	})

	It("reports a miss when no sheet prices the model", func() {
		r := pricing.NewResolver(pricing.Default())
		_, ok := r.Price("cohere", "command-r", 100, 100)
		Expect(ok).To(BeFalse())
	})

	It("is safe to call on a nil resolver", func() {
		var r *pricing.Resolver
		_, ok := r.Price("anthropic", "opus", 1, 1)
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("LoadFile", func() {
	It("parses a YAML custom price sheet", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "prices.yaml")
		body := `
anthropic:
  opus:
    input_per_1m: 20
    output_per_1m: 100
`
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())

		sheet, err := pricing.LoadFile(path)
		Expect(err).NotTo(HaveOccurred())
		p, ok := sheet.Lookup("anthropic", "claude-opus-4-5")
		Expect(ok).To(BeTrue())
		Expect(p.OutputPer1M).To(Equal(100.0))
	})

	It("errors on a missing file", func() {
		_, err := pricing.LoadFile(filepath.Join(GinkgoT().TempDir(), "nope.yaml"))
		Expect(err).To(HaveOccurred())
	})
})

const modelsDevBody = `{
  "anthropic": {"id":"anthropic","models":{
    "claude-opus-4-5":{"cost":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25}},
    "some-free-model":{"cost":{"input":0,"output":0}}
  }},
  "google": {"id":"google","models":{
    "gemini-2.5-pro":{"cost":{"input":1.25,"output":10}}
  }},
  "cohere": {"id":"cohere","models":{
    "command-r":{"cost":{"input":1,"output":2}}
  }}
}`

var _ = Describe("LoadRemote", func() {
	newServer := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}

	It("fetches, maps provider aliases, skips zero-cost models, and caches", func() {
		srv := newServer(http.StatusOK, modelsDevBody)
		defer srv.Close()

		dir := GinkgoT().TempDir()
		cache := filepath.Join(dir, "pricing.json")

		sheet, err := pricing.LoadRemote(context.Background(), pricing.RemoteOptions{
			URL:       srv.URL,
			CachePath: cache,
			Client:    srv.Client(),
		})
		Expect(err).NotTo(HaveOccurred())

		// anthropic mapped through directly.
		p, ok := sheet.Lookup("anthropic", "claude-opus-4-5")
		Expect(ok).To(BeTrue())
		Expect(p.InputPer1M).To(Equal(5.0))
		Expect(p.CacheReadPer1M).To(Equal(0.5))

		// google -> gemini alias.
		_, ok = sheet.Lookup("gemini", "gemini-2.5-pro")
		Expect(ok).To(BeTrue())

		// cohere has no inferenced provider and is dropped.
		_, ok = sheet.Lookup("cohere", "command-r")
		Expect(ok).To(BeFalse())

		// zero-cost model is skipped so it cannot shadow a default with $0.
		_, ok = sheet.Lookup("anthropic", "some-free-model")
		Expect(ok).To(BeFalse())

		// payload was cached to disk.
		_, statErr := os.Stat(cache)
		Expect(statErr).NotTo(HaveOccurred())
	})

	It("serves a fresh cache without hitting the network", func() {
		dir := GinkgoT().TempDir()
		cache := filepath.Join(dir, "pricing.json")
		Expect(os.WriteFile(cache, []byte(modelsDevBody), 0o600)).To(Succeed())

		// URL points nowhere reachable; a fresh cache must avoid using it.
		sheet, err := pricing.LoadRemote(context.Background(), pricing.RemoteOptions{
			URL:       "http://127.0.0.1:1/never",
			CachePath: cache,
			TTL:       time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
		_, ok := sheet.Lookup("anthropic", "claude-opus-4-5")
		Expect(ok).To(BeTrue())
	})

	It("falls back to a stale cache when the fetch fails", func() {
		dir := GinkgoT().TempDir()
		cache := filepath.Join(dir, "pricing.json")
		Expect(os.WriteFile(cache, []byte(modelsDevBody), 0o600)).To(Succeed())
		// Make the cache stale.
		old := time.Now().Add(-48 * time.Hour)
		Expect(os.Chtimes(cache, old, old)).To(Succeed())

		srv := newServer(http.StatusInternalServerError, "boom")
		defer srv.Close()

		sheet, err := pricing.LoadRemote(context.Background(), pricing.RemoteOptions{
			URL:       srv.URL,
			CachePath: cache,
			TTL:       time.Hour,
			Client:    srv.Client(),
		})
		Expect(err).NotTo(HaveOccurred())
		_, ok := sheet.Lookup("anthropic", "claude-opus-4-5")
		Expect(ok).To(BeTrue())
	})

	It("returns an error when the fetch fails and no cache exists", func() {
		srv := newServer(http.StatusInternalServerError, "boom")
		defer srv.Close()

		_, err := pricing.LoadRemote(context.Background(), pricing.RemoteOptions{
			URL:       srv.URL,
			CachePath: filepath.Join(GinkgoT().TempDir(), "absent.json"),
			Client:    srv.Client(),
		})
		Expect(err).To(HaveOccurred())
	})
})
