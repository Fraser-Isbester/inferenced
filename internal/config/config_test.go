package config_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/config"
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
})
