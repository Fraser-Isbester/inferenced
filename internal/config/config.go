// Package config loads the optional inferenced YAML configuration and builds the
// provider registry and identity overrides from it.
//
// When no config file is found, sensible defaults apply: all built-in providers
// enabled and the gateway listening on 127.0.0.1:8766. The headline feature is
// the openai_compatible list, which turns any OpenAI-wire endpoint (Azure,
// OpenRouter, Groq, Together, vLLM, LM Studio, ...) into a metered provider with
// no code changes.
package config

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Fraser-Isbester/inferenced/internal/enforce"
	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/gemini"
	"github.com/Fraser-Isbester/inferenced/internal/provider/ollama"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openai"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaicompat"
)

// defaultListen is the address used when config omits one.
const defaultListen = "127.0.0.1:8766"

// Config is the parsed inferenced configuration.
type Config struct {
	Listen      string      `yaml:"listen"`
	Developer   Developer   `yaml:"developer"`
	Providers   Providers   `yaml:"providers"`
	Storage     Storage     `yaml:"storage"`
	Enforcement Enforcement `yaml:"enforcement"`
}

// Storage configures the local SQLite spend ledger.
type Storage struct {
	// Enabled toggles the ledger. A nil value means enabled (the default);
	// set it to false to disable persistence entirely.
	Enabled *bool `yaml:"enabled"`
	// Path is the SQLite database file. Empty means use the default location.
	Path string `yaml:"path"`
}

// Enforcement configures local spend-limit enforcement.
type Enforcement struct {
	// Enabled turns spend enforcement on. Enforcement requires the ledger.
	Enabled bool `yaml:"enabled"`
	// Action selects the over-limit behavior: "block" (the default) or "warn".
	Action string `yaml:"action"`
	// BlockStatus is the HTTP status returned when blocking. Defaults to 402
	// (Payment Required) — coding agents read that as a quota/billing error
	// rather than an auth failure (401/403 make Claude Code prompt /login) or a
	// retryable one (429).
	BlockStatus int `yaml:"block_status"`
	// Limits is the set of spend limits to evaluate on every request.
	Limits []LimitYAML `yaml:"limits"`
}

// LimitYAML is the YAML shape of a single spend limit.
type LimitYAML struct {
	// Scope is the attribution dimension: "team" or "developer".
	Scope string `yaml:"scope"`
	// ID is the specific team or developer, or "*" to match all.
	ID string `yaml:"id"`
	// Period is the budget window: "day", "month", or "rolling".
	Period string `yaml:"period"`
	// Window is the rolling-window duration (e.g. "168h"); required only when
	// Period is "rolling".
	Window string `yaml:"window"`
	// LimitUSD is the spend ceiling in US dollars.
	LimitUSD float64 `yaml:"limit_usd"`
}

// Developer holds identity attribution overrides.
type Developer struct {
	// ID overrides the anonymized credential hash for attribution (e.g. an email).
	ID string `yaml:"id"`
	// Team is an optional team name attached to every event.
	Team string `yaml:"team"`
}

// Providers toggles built-in providers and defines OpenAI-compatible endpoints.
type Providers struct {
	Anthropic Toggle       `yaml:"anthropic"`
	OpenAI    Toggle       `yaml:"openai"`
	Gemini    Toggle       `yaml:"gemini"`
	Ollama    OllamaToggle `yaml:"ollama"`

	OpenAICompatible []CompatEntry `yaml:"openai_compatible"`
}

// Toggle enables or disables a built-in provider. A nil Enabled means enabled.
type Toggle struct {
	Enabled *bool `yaml:"enabled"`
}

// OllamaToggle is a Toggle plus an optional custom upstream address.
type OllamaToggle struct {
	Enabled  *bool  `yaml:"enabled"`
	Upstream string `yaml:"upstream"`
}

// CompatEntry defines one OpenAI-compatible endpoint.
type CompatEntry struct {
	Name     string                `yaml:"name"`
	Host     string                `yaml:"host"`
	Scheme   string                `yaml:"scheme"`
	BasePath string                `yaml:"base_path"`
	Pricing  map[string]PriceEntry `yaml:"pricing"`
}

// PriceEntry is a per-1M-token price pair in USD.
type PriceEntry struct {
	InputPer1M  float64 `yaml:"input_per_1m"`
	OutputPer1M float64 `yaml:"output_per_1m"`
}

// Default returns the built-in default configuration (all providers enabled).
func Default() *Config {
	return &Config{Listen: defaultListen}
}

// Load reads configuration from path. If path is empty it searches the default
// locations and falls back to Default when none exist. A path that is set but
// missing or malformed is an error.
func Load(path string) (*Config, error) {
	if path == "" {
		found, ok := discover()
		if !ok {
			return Default(), nil
		}
		path = found
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := Default()
	if err := yaml.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	return cfg, nil
}

// Save writes the config to path as YAML, creating any missing parent
// directories. It is the seam used by config init and the future editable UI.
func (c *Config) Save(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// discover returns the first existing config file among the default locations.
func discover() (string, bool) {
	candidates := []string{"inferenced.yaml"}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "inferenced", "config.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "inferenced", "config.yaml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, true
		}
	}
	return "", false
}

// Registry builds the provider registry described by the config, honoring the
// built-in toggles and registering each openai_compatible endpoint.
func (c *Config) Registry() *provider.Registry {
	reg := provider.NewRegistry()

	if enabled(c.Providers.Anthropic.Enabled) {
		reg.Register(anthropic.New())
	}
	if enabled(c.Providers.OpenAI.Enabled) {
		reg.Register(openai.New())
	}
	if enabled(c.Providers.Gemini.Enabled) {
		reg.Register(gemini.New())
	}
	if enabled(c.Providers.Ollama.Enabled) {
		reg.Register(ollama.NewWithUpstream(c.Providers.Ollama.Upstream))
	}

	for _, e := range c.Providers.OpenAICompatible {
		if e.Name == "" || e.Host == "" {
			continue
		}
		reg.Register(openaicompat.New(openaicompat.Config{
			Name:     e.Name,
			Host:     e.Host,
			Scheme:   e.Scheme,
			BasePath: e.BasePath,
			Pricing:  toPricing(e.Pricing),
		}))
	}

	return reg
}

// StorageEnabled reports whether the local ledger should run. It is on unless
// Storage.Enabled is explicitly set to false.
func (c *Config) StorageEnabled() bool {
	return enabled(c.Storage.Enabled)
}

// EnforceLimits maps the configured LimitYAML entries into enforce.Limit values.
// It validates that each scope is one of {team, developer} and each period is
// one of {day, month, rolling}, and parses Window (required only for rolling
// periods) via time.ParseDuration.
func (c *Config) EnforceLimits() ([]enforce.Limit, error) {
	switch c.Enforcement.Action {
	case "", string(enforce.ActionBlock), string(enforce.ActionWarn):
	default:
		return nil, fmt.Errorf("invalid enforcement action %q (want block or warn)", c.Enforcement.Action)
	}

	limits := make([]enforce.Limit, 0, len(c.Enforcement.Limits))
	for i, l := range c.Enforcement.Limits {
		switch l.Scope {
		case string(enforce.ScopeTeam), string(enforce.ScopeDeveloper):
		default:
			return nil, fmt.Errorf("limit %d: invalid scope %q (want team or developer)", i, l.Scope)
		}

		switch l.Period {
		case string(enforce.PeriodDay), string(enforce.PeriodMonth), string(enforce.PeriodRolling):
		default:
			return nil, fmt.Errorf("limit %d: invalid period %q (want day, month, or rolling)", i, l.Period)
		}

		var window time.Duration
		if l.Period == string(enforce.PeriodRolling) {
			if l.Window == "" {
				return nil, fmt.Errorf("limit %d: window is required for rolling period", i)
			}
			d, err := time.ParseDuration(l.Window)
			if err != nil {
				return nil, fmt.Errorf("limit %d: parse window %q: %w", i, l.Window, err)
			}
			window = d
		}

		limits = append(limits, enforce.Limit{
			Scope:    enforce.Scope(l.Scope),
			ID:       l.ID,
			Period:   enforce.Period(l.Period),
			Window:   window,
			LimitUSD: l.LimitUSD,
		})
	}
	return limits, nil
}

// EnforceAction returns the configured over-limit action, defaulting to block.
// EnforceLimits validates the raw value, so callers should invoke it first.
func (c *Config) EnforceAction() enforce.Action {
	if c.Enforcement.Action == "" {
		return enforce.ActionBlock
	}
	return enforce.Action(c.Enforcement.Action)
}

// BlockStatus returns the HTTP status used when blocking, defaulting to 402.
func (c *Config) BlockStatus() int {
	if c.Enforcement.BlockStatus == 0 {
		return http.StatusPaymentRequired
	}
	return c.Enforcement.BlockStatus
}

// enabled reports whether a nil-defaults-true toggle is on.
func enabled(b *bool) bool { return b == nil || *b }

// toPricing converts config price entries into the openaicompat pricing map.
func toPricing(in map[string]PriceEntry) map[string]openaicompat.Price {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]openaicompat.Price, len(in))
	for k, v := range in {
		out[k] = openaicompat.Price{InputPer1M: v.InputPer1M, OutputPer1M: v.OutputPer1M}
	}
	return out
}
