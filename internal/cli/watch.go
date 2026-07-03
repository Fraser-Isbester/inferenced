package cli

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/enforce"
)

// configPollInterval is how often watchConfig stats the config file. It is a
// var so tests can shorten it.
var configPollInterval = 2 * time.Second

// buildRuleset turns a config into the enforcement Ruleset used by the gateway.
// Disabled enforcement yields an empty engine (Check allows everything), so
// toggling enforcement.enabled via a config edit takes effect on reload. spend
// is the ledger-backed spend source injected into the engine.
func buildRuleset(c *config.Config, spend enforce.SpendFunc) (enforce.Ruleset, error) {
	var limits []enforce.Limit
	if c.Enforcement.Enabled {
		ls, err := c.EnforceLimits()
		if err != nil {
			return enforce.Ruleset{}, err
		}
		limits = ls
	}
	return enforce.Ruleset{
		Engine:      enforce.NewEngine(limits, spend, time.Now),
		Action:      c.EnforceAction(),
		BlockStatus: c.BlockStatus(),
		Rules:       c.DowngradeRules(),
	}, nil
}

// watchConfig polls path for modification-time changes and invokes onChange
// with the freshly loaded config on each change, until ctx is cancelled. It
// never fires for the file's state at startup (only subsequent edits), tolerates
// a missing file (waits for it to appear), and logs+skips load errors so a bad
// edit never crashes the gateway.
func watchConfig(ctx context.Context, path string, onChange func(*config.Config)) {
	if path == "" {
		return
	}
	var last time.Time
	if fi, err := os.Stat(path); err == nil {
		last = fi.ModTime()
	}

	ticker := time.NewTicker(configPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue // not present yet or transient
			}
			if !fi.ModTime().After(last) {
				continue
			}
			last = fi.ModTime()
			cfg, err := config.Load(path)
			if err != nil {
				log.Printf("config reload: load %s: %v", path, err)
				continue
			}
			onChange(cfg)
		}
	}
}
