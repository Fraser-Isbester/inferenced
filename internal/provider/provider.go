// Package provider defines the provider-agnostic abstraction used by the
// inferenced proxy to detect, parse, and price LLM inference traffic.
//
// A Provider knows how to recognize its own traffic (by upstream host or by
// request heuristics), extract request metadata, incrementally parse a streamed
// response to meter token usage, and compute a USD cost. Concrete providers
// (Anthropic, OpenAI, etc.) live in their own sub-packages and register
// themselves with a Registry, keeping the design add-a-file extensible.
package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/artifact"
)

// RequestInfo captures the metadata extracted from an inference request body.
type RequestInfo struct {
	// Model is the requested model identifier.
	Model string
	// ApproxInputTokens is a heuristic estimate of the input token count.
	ApproxInputTokens int
	// Stream reports whether the client requested a streamed response.
	Stream bool
	// Metadata is an optional bounded blob of coding-agent signals extracted
	// from the request body (tools, tool-call counts, files, commands, cwd/git
	// context, message count) for later usage analytics. Nil when nothing was
	// captured; it never contains raw prompt or file-content text.
	Metadata map[string]any
	// Artifacts are the linkable business outcomes (PRs, commits, pushes, …)
	// produced during the request, derived from tool calls and their results.
	// Nil when none were produced.
	Artifacts []artifact.Artifact
}

// Usage holds the token accounting for a single inference response.
type Usage struct {
	// InputTokens is the number of prompt/input tokens.
	InputTokens int
	// OutputTokens is the number of completion/output tokens.
	OutputTokens int
	// Approximated is true when any field was estimated rather than reported
	// authoritatively by the upstream provider.
	Approximated bool
}

// StreamParser is fed the tee'd response bytes via its io.Writer interface as
// they stream by. Implementations must NOT block on heavy work in Write, since
// that back-pressures the client. Finish is called once at EOF and returns the
// accumulated usage.
type StreamParser interface {
	io.Writer
	// Finish is called once after the response body reaches EOF and returns the
	// accumulated usage for the response.
	Finish() Usage
}

// Provider abstracts a single LLM inference backend. Implementations must be
// safe for concurrent use across requests.
type Provider interface {
	// Name returns the stable provider identifier, e.g. "anthropic" or "openai".
	Name() string
	// UpstreamHost returns the real destination host, e.g. "api.anthropic.com".
	UpstreamHost() string
	// UpstreamScheme returns the scheme used to reach the upstream, "https" for
	// hosted APIs or "http" for local endpoints (e.g. Ollama, a local vLLM).
	UpstreamScheme() string
	// Hosts returns the hostnames that identify this provider for Host-first
	// detection.
	Hosts() []string
	// Matches reports whether the request looks like this provider's traffic,
	// using path and header heuristics (used as an env-override mode fallback).
	Matches(r *http.Request) bool
	// ParseRequest extracts request metadata from the request. The body is passed
	// separately (already read and restorable); r is provided because some
	// providers carry the model in the URL path rather than the body.
	ParseRequest(r *http.Request, body []byte) RequestInfo
	// NewStreamParser returns a StreamParser suitable for the given response
	// Content-Type.
	NewStreamParser(contentType string) StreamParser
	// Price returns the cost in USD for the given model and token counts.
	Price(model string, inTok, outTok int) float64
	// ErrorResponse renders a provider-native error envelope for the given HTTP
	// status and message, returning the JSON Content-Type and marshaled body.
	ErrorResponse(status int, message string) (contentType string, body []byte)
	// RewriteModel reconfigures the request to use newModel, for real-time model
	// down-scoping. It returns the body to forward: body-based providers re-encode
	// the JSON "model" field, while path-based providers (Gemini) mutate r.URL and
	// return the body unchanged. The original body is returned on any parse error.
	RewriteModel(r *http.Request, body []byte, newModel string) ([]byte, error)
}

// RewriteBodyModel returns a copy of a JSON request body with its top-level
// "model" field set to newModel, preserving every other field. It is the shared
// implementation for body-based providers (Anthropic, OpenAI-wire, Ollama). On a
// parse error it returns the original body unchanged alongside the error so the
// caller can forward the request rather than drop it.
func RewriteBodyModel(body []byte, newModel string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, fmt.Errorf("rewrite model: parse body: %w", err)
	}
	if obj == nil {
		obj = make(map[string]json.RawMessage, 1)
	}
	m, err := json.Marshal(newModel)
	if err != nil {
		return body, fmt.Errorf("rewrite model: encode model: %w", err)
	}
	obj["model"] = m
	out, err := json.Marshal(obj)
	if err != nil {
		return body, fmt.Errorf("rewrite model: encode body: %w", err)
	}
	return out, nil
}

// Registry holds the set of registered providers and resolves which one should
// handle a given request.
type Registry struct {
	providers []Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a provider to the registry. Registration order determines the
// fallback priority used by Detect's Matches phase.
func (r *Registry) Register(p Provider) {
	r.providers = append(r.providers, p)
}

// Names returns the registered provider names in registration order.
func (r *Registry) Names() []string {
	names := make([]string, len(r.providers))
	for i, p := range r.providers {
		names[i] = p.Name()
	}
	return names
}

// Detect resolves the provider for a request. It first tries Host-first
// detection: if the request Host matches any provider's Hosts, that provider is
// returned. Otherwise it returns the first provider whose Matches reports true.
// If neither succeeds it returns (nil, false).
func (r *Registry) Detect(req *http.Request) (Provider, bool) {
	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	host = stripPort(host)

	for _, p := range r.providers {
		for _, h := range p.Hosts() {
			if strings.EqualFold(host, h) {
				return p, true
			}
		}
	}

	for _, p := range r.providers {
		if p.Matches(req) {
			return p, true
		}
	}

	return nil, false
}

// stripPort removes a trailing ":port" from a host, if present.
func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i != -1 {
		// Guard against IPv6 literals without a port such as "::1"; a real
		// host:port always has the colon after the closing bracket or after a
		// bare hostname, so only strip when what follows is all digits.
		if !strings.ContainsRune(host[i+1:], ':') {
			return host[:i]
		}
	}
	return host
}
