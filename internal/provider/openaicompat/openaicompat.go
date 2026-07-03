// Package openaicompat implements a config-defined provider for any endpoint
// that speaks the OpenAI wire format (Azure OpenAI, OpenRouter, Groq, Together,
// Fireworks, vLLM, LM Studio, ...).
//
// A single config entry — name, host, scheme, and optional pricing — turns a
// whole OpenAI-compatible endpoint into a metered provider with no new code.
// Wire-format parsing is shared with the first-party OpenAI provider via
// openaiwire.
package openaicompat

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

// Price holds the per-1M-token cost in USD for a model (or the default).
type Price struct {
	InputPer1M  float64
	OutputPer1M float64
}

// Config describes one OpenAI-compatible endpoint.
type Config struct {
	// Name is the provider identifier used in telemetry, e.g. "openrouter".
	Name string
	// Host is the upstream host (and optional port), e.g. "openrouter.ai".
	Host string
	// Scheme is "https" (default) or "http" for local endpoints.
	Scheme string
	// BasePath, if set, scopes path-based Matches, e.g. "/api/v1".
	BasePath string
	// Pricing maps a lowercase model substring to a price; the "default" key is
	// the fallback. Absent pricing yields $0.
	Pricing map[string]Price
}

// compatProvider is a config-driven OpenAI-compatible provider.
type compatProvider struct {
	cfg Config
}

// New returns a provider.Provider for the given OpenAI-compatible endpoint.
func New(cfg Config) provider.Provider {
	if cfg.Scheme == "" {
		cfg.Scheme = "https"
	}
	return compatProvider{cfg: cfg}
}

// Name returns the configured provider identifier.
func (p compatProvider) Name() string { return p.cfg.Name }

// UpstreamHost returns the configured host.
func (p compatProvider) UpstreamHost() string { return p.cfg.Host }

// UpstreamScheme returns the configured scheme.
func (p compatProvider) UpstreamScheme() string { return p.cfg.Scheme }

// Hosts returns the configured host for Host-first detection.
func (p compatProvider) Hosts() []string { return []string{p.cfg.Host} }

// Matches reports whether the request path looks like an OpenAI-style endpoint,
// optionally scoped to the configured base path.
func (p compatProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	if p.cfg.BasePath != "" && !strings.HasPrefix(path, p.cfg.BasePath) {
		return false
	}
	return strings.HasSuffix(path, "/chat/completions") ||
		strings.HasSuffix(path, "/responses") ||
		strings.HasSuffix(path, "/embeddings")
}

// ParseRequest extracts request metadata via the shared OpenAI wire parser.
func (compatProvider) ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	return openaiwire.ParseRequest(r, body)
}

// NewStreamParser returns the shared OpenAI wire stream parser.
func (compatProvider) NewStreamParser(contentType string) provider.StreamParser {
	return openaiwire.NewParser(contentType)
}

// Price returns the configured cost, matching the model by substring and
// falling back to the "default" entry, or $0 when no pricing is configured.
func (p compatProvider) Price(model string, inTok, outTok int) float64 {
	pr, ok := p.lookup(model)
	if !ok {
		return 0
	}
	return float64(inTok)/1e6*pr.InputPer1M + float64(outTok)/1e6*pr.OutputPer1M
}

// lookup resolves the price for a model: first a substring match, then the
// "default" key.
func (p compatProvider) lookup(model string) (Price, bool) {
	m := strings.ToLower(model)
	for key, pr := range p.cfg.Pricing {
		if key == "default" {
			continue
		}
		if strings.Contains(m, strings.ToLower(key)) {
			return pr, true
		}
	}
	if pr, ok := p.cfg.Pricing["default"]; ok {
		return pr, true
	}
	return Price{}, false
}

// ErrorResponse renders the shared OpenAI-style error envelope. The error type
// is "insufficient_quota" for HTTP 403 and "api_error" otherwise.
func (compatProvider) ErrorResponse(status int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    openaiwire.ErrType(status),
			"code":    nil,
		},
	})
	return "application/json", body
}
