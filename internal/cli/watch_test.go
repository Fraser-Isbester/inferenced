package cli

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/config"
)

var noSpend = func(string, string, time.Time) (float64, error) { return 0, nil }

var _ = Describe("buildRuleset", func() {
	It("yields an allow-everything engine when enforcement is disabled", func() {
		cfg := config.Default() // enforcement off
		rs, err := buildRuleset(cfg, noSpend)
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Engine.Check("dev", "team").Allowed).To(BeTrue())
	})

	It("builds a blocking engine from enabled limits", func() {
		cfg := config.Default()
		cfg.Enforcement = config.Enforcement{
			Enabled: true,
			Limits:  []config.LimitYAML{{Scope: "developer", ID: "*", Period: "day", LimitUSD: 1}},
		}
		rs, err := buildRuleset(cfg, func(string, string, time.Time) (float64, error) { return 99, nil })
		Expect(err).NotTo(HaveOccurred())
		Expect(rs.Engine.Check("dev", "team").Allowed).To(BeFalse())
	})

	It("errors on an invalid limit", func() {
		cfg := config.Default()
		cfg.Enforcement = config.Enforcement{
			Enabled: true,
			Limits:  []config.LimitYAML{{Scope: "org", ID: "*", Period: "day", LimitUSD: 1}},
		}
		_, err := buildRuleset(cfg, noSpend)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("watchConfig", func() {
	It("invokes onChange with the reloaded config after the file changes", func() {
		orig := configPollInterval
		configPollInterval = 10 * time.Millisecond
		DeferCleanup(func() { configPollInterval = orig })

		path := filepath.Join(GinkgoT().TempDir(), "inferenced.yaml")
		Expect(os.WriteFile(path, []byte("listen: \"127.0.0.1:1\"\n"), 0o600)).To(Succeed())

		got := make(chan *config.Config, 1)
		ctx, cancel := context.WithCancel(context.Background())
		DeferCleanup(cancel)
		go watchConfig(ctx, path, func(c *config.Config) {
			select {
			case got <- c:
			default:
			}
		})

		// Advance the mtime forward each poll until the watcher observes a change.
		// Monotonic timestamps make this immune to whenever the watcher's initial
		// stat happened to run.
		future := time.Now()
		Eventually(func() bool {
			future = future.Add(time.Second)
			_ = os.Chtimes(path, future, future)
			select {
			case <-got:
				return true
			default:
				return false
			}
		}, 3*time.Second, 30*time.Millisecond).Should(BeTrue())
	})
})
