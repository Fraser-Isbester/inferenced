// Package pricing resolves the USD cost of an inference request from a layered
// set of price sheets.
//
// A Sheet maps a provider name to a set of model matchers and their per-1M-token
// prices. The same schema is used for three sources, consulted in priority order
// by a Resolver:
//
//  1. a user-supplied custom price sheet (highest priority — authoritative),
//  2. a live sheet fetched from models.dev on boot and cached to disk,
//  3. the embedded default sheet (this package's default.json), which
//     consolidates the family-level approximations that used to live in each
//     provider.
//
// When no sheet prices a model, the Resolver reports miss and the caller falls
// back to the provider's own built-in Price method. There is no official
// per-provider pricing API, so "live" pricing means the community-maintained
// models.dev aggregate rather than a first-party feed.
package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed default.json
var defaultSheetJSON []byte

// Price is the per-1M-token cost in USD for a model (or a provider default).
// Cache prices are optional (zero means unspecified) and are recorded for
// forward use; cost computation currently uses only input and output tokens.
type Price struct {
	InputPer1M      float64 `json:"input_per_1m" yaml:"input_per_1m"`
	OutputPer1M     float64 `json:"output_per_1m" yaml:"output_per_1m"`
	CacheReadPer1M  float64 `json:"cache_read_per_1m,omitempty" yaml:"cache_read_per_1m,omitempty"`
	CacheWritePer1M float64 `json:"cache_write_per_1m,omitempty" yaml:"cache_write_per_1m,omitempty"`
}

// Sheet is a price lookup keyed by provider name, then by a lowercase model
// matcher. The matcher is either a substring of the model id or the literal
// "default" used when no substring matches.
type Sheet struct {
	// providers maps provider name -> (model matcher | "default") -> Price.
	providers map[string]map[string]Price
}

// NewSheet builds a Sheet from a nested map. Provider names and model matchers
// are lowercased so lookups are case-insensitive. A nil or empty map yields an
// empty (never nil) Sheet.
func NewSheet(in map[string]map[string]Price) *Sheet {
	providers := make(map[string]map[string]Price, len(in))
	for prov, models := range in {
		lowered := make(map[string]Price, len(models))
		for matcher, price := range models {
			lowered[strings.ToLower(matcher)] = price
		}
		providers[strings.ToLower(prov)] = lowered
	}
	return &Sheet{providers: providers}
}

// Lookup returns the price for a provider/model pair. It prefers an exact model
// match, then the longest model matcher that is a substring of the model, then
// the provider's "default" entry. The bool is false when the provider is
// unknown or has no matching or default entry.
func (s *Sheet) Lookup(provider, model string) (Price, bool) {
	if s == nil {
		return Price{}, false
	}
	byModel, ok := s.providers[strings.ToLower(provider)]
	if !ok || len(byModel) == 0 {
		return Price{}, false
	}
	m := strings.ToLower(model)

	if p, ok := byModel[m]; ok {
		return p, true
	}

	// Longest substring match wins so "gpt-4o-mini" beats "gpt-4o".
	best := ""
	var bestPrice Price
	for matcher, p := range byModel {
		if matcher == "default" {
			continue
		}
		if strings.Contains(m, matcher) && len(matcher) > len(best) {
			best = matcher
			bestPrice = p
		}
	}
	if best != "" {
		return bestPrice, true
	}

	if p, ok := byModel["default"]; ok {
		return p, true
	}
	return Price{}, false
}

// Cost returns the input+output cost in USD for the given provider/model and
// token counts. The bool mirrors Lookup: false when the sheet cannot price it.
func (s *Sheet) Cost(provider, model string, inTok, outTok int) (float64, bool) {
	p, ok := s.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	cost := float64(inTok)/1e6*p.InputPer1M + float64(outTok)/1e6*p.OutputPer1M
	return cost, true
}

// Empty reports whether the sheet prices nothing.
func (s *Sheet) Empty() bool {
	return s == nil || len(s.providers) == 0
}

// Default returns the embedded default sheet consolidating the built-in
// family-level approximations. It panics only if the embedded JSON is malformed,
// which is a build-time invariant covered by tests.
func Default() *Sheet {
	var raw map[string]map[string]Price
	if err := json.Unmarshal(defaultSheetJSON, &raw); err != nil {
		panic(fmt.Sprintf("pricing: embedded default.json is invalid: %v", err))
	}
	return NewSheet(raw)
}

// LoadFile reads a custom price sheet from path. The file may be YAML or JSON
// (YAML is a JSON superset, so the YAML decoder parses both); its shape is the
// same nested provider -> matcher -> Price map as the embedded default.
func LoadFile(path string) (*Sheet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read price sheet %s: %w", path, err)
	}
	var raw map[string]map[string]Price
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse price sheet %s: %w", path, err)
	}
	return NewSheet(raw), nil
}

// Resolver prices a request from an ordered list of sheets, highest priority
// first. Price returns the first sheet that can price the model.
type Resolver struct {
	sheets []*Sheet
}

// NewResolver builds a Resolver from sheets in priority order. Nil and empty
// sheets are skipped.
func NewResolver(sheets ...*Sheet) *Resolver {
	kept := make([]*Sheet, 0, len(sheets))
	for _, s := range sheets {
		if !s.Empty() {
			kept = append(kept, s)
		}
	}
	return &Resolver{sheets: kept}
}

// Price returns the cost in USD for the provider/model, consulting each sheet in
// priority order. The bool is false when no sheet prices the model, signaling
// the caller to fall back to the provider's built-in price.
func (r *Resolver) Price(provider, model string, inTok, outTok int) (float64, bool) {
	if r == nil {
		return 0, false
	}
	for _, s := range r.sheets {
		if cost, ok := s.Cost(provider, model, inTok, outTok); ok {
			return cost, true
		}
	}
	return 0, false
}

// cachePathFallback returns the default on-disk cache location for the fetched
// remote sheet: $XDG_CACHE_HOME/inferenced/pricing.json, falling back to
// ~/.cache/inferenced/pricing.json.
func cachePathFallback() (string, error) {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "inferenced", "pricing.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "inferenced", "pricing.json"), nil
}
