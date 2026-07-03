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
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

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
	Listen    string    `yaml:"listen"`
	Developer Developer `yaml:"developer"`
	Providers Providers `yaml:"providers"`
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
