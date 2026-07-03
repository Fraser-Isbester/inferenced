// Package openai implements the provider.Provider abstraction for OpenAI's
// inference API (chat completions, responses, and embeddings).
//
// Wire-format parsing is shared with config-defined OpenAI-compatible endpoints
// via the openaiwire package; this package adds OpenAI's host, detection
// heuristics, and pricing.
package openai

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

// provider name and upstream host constants.
const (
	name         = "openai"
	upstreamHost = "api.openai.com"
)

// openaiProvider is the concrete provider.Provider implementation for OpenAI.
type openaiProvider struct{}

// New returns a provider.Provider for OpenAI.
func New() provider.Provider { return openaiProvider{} }

// Name returns the stable provider identifier.
func (openaiProvider) Name() string { return name }

// UpstreamHost returns the real OpenAI API host.
func (openaiProvider) UpstreamHost() string { return upstreamHost }

// UpstreamScheme returns the scheme used to reach OpenAI.
func (openaiProvider) UpstreamScheme() string { return "https" }

// Hosts returns the hostnames that identify OpenAI traffic for Host-first
// detection.
func (openaiProvider) Hosts() []string { return []string{upstreamHost} }

// Matches reports whether the request path looks like an OpenAI inference
// endpoint (chat completions, responses, or embeddings).
func (openaiProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := r.URL.Path
	return strings.HasSuffix(p, "/chat/completions") ||
		strings.HasSuffix(p, "/responses") ||
		strings.HasSuffix(p, "/embeddings")
}

// ParseRequest extracts request metadata via the shared OpenAI wire parser.
func (openaiProvider) ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	return openaiwire.ParseRequest(r, body)
}

// NewStreamParser returns the shared OpenAI wire stream parser.
func (openaiProvider) NewStreamParser(contentType string) provider.StreamParser {
	return openaiwire.NewParser(contentType)
}

// Pricing per 1M tokens in USD. These are approximate spike defaults, not a
// source of truth for billing.
type price struct {
	inPerM, outPerM float64
}

var (
	priceGPT4o     = price{inPerM: 2.50, outPerM: 10.0}
	priceGPT4oMini = price{inPerM: 0.15, outPerM: 0.60}
	priceDefault   = price{inPerM: 2.50, outPerM: 10.0}
)

// Price returns the approximate cost in USD for the given model and token
// counts, matching the model by substring.
func (openaiProvider) Price(model string, inTok, outTok int) float64 {
	m := strings.ToLower(model)
	pr := priceDefault
	switch {
	case strings.Contains(m, "gpt-4o-mini"):
		pr = priceGPT4oMini
	case strings.Contains(m, "gpt-4o"):
		pr = priceGPT4o
	}
	return float64(inTok)/1e6*pr.inPerM + float64(outTok)/1e6*pr.outPerM
}

// ErrorResponse renders OpenAI's native error envelope. The error type is
// "insufficient_quota" for HTTP 403 and "api_error" otherwise.
func (openaiProvider) ErrorResponse(status int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    openaiwire.ErrType(status),
			"code":    nil,
		},
	})
	return "application/json", body
}
