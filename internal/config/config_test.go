package config_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/enforce"
)

func TestConfig(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Config Suite")
}

var _ = Describe("Default", func() {
	It("enables the built-in providers and listens on the default address", func() {
		cfg := config.Default()
		Expect(cfg.Listen).To(Equal("127.0.0.1:8766"))
		reg := cfg.Registry()

		// anthropic is detectable by its version header.
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("anthropic-version", "2023-06-01")
		got, ok := reg.Detect(r)
		Expect(ok).To(BeTrue())
		Expect(got.Name()).To(Equal("anthropic"))
	})
})

var _ = Describe("Load", func() {
	writeConfig := func(body string) string {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "inferenced.yaml")
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
		return path
	}

	It("parses identity, disables a built-in, and registers an openai_compatible endpoint", func() {
		path := writeConfig(`
listen: "127.0.0.1:9999"
developer:
  id: "alice@example.com"
  team: "platform"
providers:
  openai: { enabled: false }
  openai_compatible:
    - { name: openrouter, host: openrouter.ai, base_path: /api/v1 }
`)
		cfg, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Listen).To(Equal("127.0.0.1:9999"))
		Expect(cfg.Developer.ID).To(Equal("alice@example.com"))
		Expect(cfg.Developer.Team).To(Equal("platform"))

		reg := cfg.Registry()

		// The config-defined endpoint is detectable by host.
		byHost := httptest.NewRequest("POST", "/api/v1/chat/completions", nil)
		byHost.Host = "openrouter.ai"
		got, ok := reg.Detect(byHost)
		Expect(ok).To(BeTrue())
		Expect(got.Name()).To(Equal("openrouter"))

		// openai was disabled: a bare /v1/chat/completions to an unknown host
		// is no longer matched.
		disabled := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		disabled.Host = "example.invalid"
		_, ok = reg.Detect(disabled)
		Expect(ok).To(BeFalse())
	})

	It("errors when an explicit path is missing", func() {
		_, err := config.Load(filepath.Join(GinkgoT().TempDir(), "nope.yaml"))
		Expect(err).To(HaveOccurred())
	})

	It("parses storage and enforcement with all three period types", func() {
		path := writeConfig(`
storage:
  enabled: true
  path: "/var/tmp/ledger.db"
enforcement:
  enabled: true
  action: warn
  limits:
    - { scope: developer, id: "*", period: day, limit_usd: 5 }
    - { scope: team, id: platform, period: month, limit_usd: 100 }
    - { scope: team, id: "*", period: rolling, window: 168h, limit_usd: 250 }
`)
		cfg, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.StorageEnabled()).To(BeTrue())
		Expect(cfg.Storage.Path).To(Equal("/var/tmp/ledger.db"))
		Expect(cfg.Enforcement.Enabled).To(BeTrue())
		Expect(cfg.Enforcement.Action).To(Equal("warn"))

		limits, err := cfg.EnforceLimits()
		Expect(err).NotTo(HaveOccurred())
		Expect(limits).To(HaveLen(3))

		Expect(limits[0].Scope).To(Equal(enforce.ScopeDeveloper))
		Expect(limits[0].ID).To(Equal("*"))
		Expect(limits[0].Period).To(Equal(enforce.PeriodDay))
		Expect(limits[0].Window).To(BeZero())
		Expect(limits[0].LimitUSD).To(Equal(5.0))

		Expect(limits[1].Scope).To(Equal(enforce.ScopeTeam))
		Expect(limits[1].ID).To(Equal("platform"))
		Expect(limits[1].Period).To(Equal(enforce.PeriodMonth))
		Expect(limits[1].LimitUSD).To(Equal(100.0))

		Expect(limits[2].Period).To(Equal(enforce.PeriodRolling))
		Expect(limits[2].Window).To(Equal(168 * time.Hour))
		Expect(limits[2].LimitUSD).To(Equal(250.0))
	})
})

var _ = Describe("downgrade configuration", func() {
	writeConfig := func(body string) string {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "inferenced.yaml")
		Expect(os.WriteFile(path, []byte(body), 0o600)).To(Succeed())
		return path
	}

	It("parses per-limit thresholds and downgrade rules", func() {
		path := writeConfig(`
enforcement:
  enabled: true
  downgrade:
    - { match: opus, to: claude-sonnet-4-5 }
    - { match: sonnet, to: claude-haiku-4-5 }
  limits:
    - { scope: team, id: team-a, period: day, limit_usd: 100, downgrade_at: 0.8, block_at: 1.2 }
    - { scope: team, id: team-b, period: day, limit_usd: 100 }
`)
		cfg, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())

		limits, err := cfg.EnforceLimits()
		Expect(err).NotTo(HaveOccurred())
		Expect(limits).To(HaveLen(2))
		Expect(limits[0].DowngradeAt).To(Equal(0.8))
		Expect(limits[0].BlockAt).To(Equal(1.2))
		Expect(limits[1].DowngradeAt).To(BeZero())
		Expect(limits[1].BlockAt).To(BeZero())

		rules := cfg.DowngradeRules()
		Expect(rules).To(HaveLen(2))
		Expect(rules[0].Match).To(Equal("opus"))
		Expect(rules[0].To).To(Equal("claude-sonnet-4-5"))
	})

	It("rejects a limit whose downgrade_at is not below its block_at", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{
			Limits: []config.LimitYAML{{Scope: "team", ID: "*", Period: "day", LimitUSD: 100, DowngradeAt: 1.2, BlockAt: 1.0}},
		}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})

	It("rejects a downgrade rule missing its target", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{
			Downgrade: []config.DowngradeRuleYAML{{Match: "opus"}},
		}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("StorageEnabled", func() {
	It("defaults to true when storage is unset", func() {
		Expect(config.Default().StorageEnabled()).To(BeTrue())
	})

	It("is false when storage.enabled is explicitly false", func() {
		enabled := false
		cfg := &config.Config{Storage: config.Storage{Enabled: &enabled}}
		Expect(cfg.StorageEnabled()).To(BeFalse())
	})
})

var _ = Describe("EnforceLimits", func() {
	It("requires a window for the rolling period", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{
			Limits: []config.LimitYAML{{Scope: "team", ID: "*", Period: "rolling", LimitUSD: 10}},
		}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown scope", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{
			Limits: []config.LimitYAML{{Scope: "org", ID: "*", Period: "day", LimitUSD: 10}},
		}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown period", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{
			Limits: []config.LimitYAML{{Scope: "team", ID: "*", Period: "year", LimitUSD: 10}},
		}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})

	It("rejects an unknown action (so a typo can't silently hard-block)", func() {
		cfg := &config.Config{Enforcement: config.Enforcement{Action: "wan"}}
		_, err := cfg.EnforceLimits()
		Expect(err).To(HaveOccurred())
	})

	It("defaults an empty action to block", func() {
		cfg := &config.Config{}
		Expect(cfg.EnforceAction()).To(Equal(enforce.ActionBlock))
	})
})

var _ = Describe("Save", func() {
	It("creates parent directories and round-trips through Load", func() {
		dir := GinkgoT().TempDir()
		path := filepath.Join(dir, "nested", "inferenced.yaml")

		cfg := config.Default()
		cfg.Developer.ID = "dev-1"
		cfg.Developer.Team = "team-a"
		Expect(cfg.Save(path)).To(Succeed())

		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o644)))

		loaded, err := config.Load(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(loaded.Developer.ID).To(Equal("dev-1"))
		Expect(loaded.Developer.Team).To(Equal("team-a"))
		Expect(loaded.Listen).To(Equal(cfg.Listen))
	})
})
