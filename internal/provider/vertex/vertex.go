// Package vertex implements the provider.Provider abstraction for Google Vertex
// AI.
//
// Vertex differs from the other providers in two ways. Its hosts are a family
// ("{region}-aiplatform.googleapis.com" plus the "aiplatform.googleapis.com"
// global endpoint) rather than one fixed name, and one host multiplexes three
// wire formats chosen by the URL path:
//
//   - publishers/google/models/{m}:generateContent — Gemini wire
//   - publishers/anthropic/models/{m}:rawPredict — Anthropic Messages wire, with
//     the model in the path instead of the body
//   - endpoints/openapi/chat/completions, and other publishers' rawPredict
//     (Llama, Mistral MaaS) — OpenAI wire
//
// So this provider is a router: it resolves the route from the path and
// delegates parsing to the existing gemini, anthropic and openaiwire code.
// Auth needs nothing from us: clients mint an ADC bearer token and the proxy
// forwards headers untouched.
package vertex

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/gemini"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openaiwire"
)

const (
	name = "vertex"
	// globalHost serves the "global" location and is the fallback upstream.
	globalHost     = "aiplatform.googleapis.com"
	regionalSuffix = "-" + globalHost
)

// wire is the response/request format a route speaks.
type wire int

const (
	// wireNone covers Vertex calls that are not metered inference (countTokens,
	// Claude's count-tokens model, management APIs). They are forwarded and
	// recorded at zero tokens.
	wireNone wire = iota
	wireGemini
	wireAnthropic
	wireOpenAI
)

// route is what the URL path says about a request.
type route struct {
	location  string
	publisher string
	model     string
	method    string
	wire      wire
}

var (
	// Location is constrained to [a-z0-9-] because it becomes part of the
	// upstream hostname; anything looser would let a crafted path aim the
	// proxy at an arbitrary host.
	publisherRe = regexp.MustCompile(`/projects/[^/]+/locations/([a-z0-9-]+)/publishers/([^/]+)/models/([^/:?]+):([A-Za-z]+)`)
	openapiRe   = regexp.MustCompile(`/projects/[^/]+/locations/([a-z0-9-]+)/endpoints/openapi/(chat/completions|completions)$`)
	locationRe  = regexp.MustCompile(`/projects/[^/]+/locations/([a-z0-9-]+)(?:/|$)`)
	// labelRe validates the region prefix of a regional host.
	labelRe = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// parseRoute classifies a Vertex path. ok is false when the path is not a
// Vertex project/location path at all.
func parseRoute(path string) (route, bool) {
	if m := publisherRe.FindStringSubmatch(path); m != nil {
		rt := route{location: m[1], publisher: m[2], model: m[3], method: m[4]}
		switch {
		case rt.model == "count-tokens":
			// Claude-on-Vertex token counting is addressed as a pseudo-model.
		case rt.method == "generateContent" || rt.method == "streamGenerateContent":
			rt.wire = wireGemini
		case rt.method == "rawPredict" || rt.method == "streamRawPredict":
			if rt.publisher == "anthropic" {
				rt.wire = wireAnthropic
			} else {
				rt.wire = wireOpenAI
			}
		}
		return rt, true
	}
	if m := openapiRe.FindStringSubmatch(path); m != nil {
		return route{location: m[1], wire: wireOpenAI}, true
	}
	if m := locationRe.FindStringSubmatch(path); m != nil {
		return route{location: m[1]}, true
	}
	return route{}, false
}

// hostFor maps a location to its API host.
func hostFor(location string) string {
	if location == "" || location == "global" {
		return globalHost
	}
	return location + regionalSuffix
}

// isVertexHost reports whether host is the global or a regional Vertex host.
func isVertexHost(host string) bool {
	host = strings.ToLower(host)
	if host == globalHost {
		return true
	}
	region, ok := strings.CutSuffix(host, regionalSuffix)
	return ok && labelRe.MatchString(region)
}

var (
	geminiP    = gemini.New()
	anthropicP = anthropic.New()
)

// vertexProvider routes Vertex traffic to the right wire implementation.
type vertexProvider struct {
	// host, when set, pins every request to one upstream (e.g. a Private
	// Service Connect endpoint) instead of routing by region.
	host string
}

// New returns a Vertex provider that routes each request by its region.
func New() provider.Provider { return vertexProvider{} }

// NewWithHost returns a Vertex provider that forwards every request to host.
// An empty host behaves like New.
func NewWithHost(host string) provider.Provider { return vertexProvider{host: host} }

// Name returns the stable provider identifier.
func (vertexProvider) Name() string { return name }

// UpstreamHost returns the pinned host, or the global endpoint. Per-request
// routing goes through UpstreamFor.
func (p vertexProvider) UpstreamHost() string {
	if p.host != "" {
		return p.host
	}
	return globalHost
}

// UpstreamScheme returns the scheme used to reach Vertex.
func (vertexProvider) UpstreamScheme() string { return "https" }

// Hosts returns the static global host; regional hosts match via MatchesHost.
func (vertexProvider) Hosts() []string { return []string{globalHost} }

// MatchesHost claims the whole regional host family for Host-first detection
// (transparent interception).
func (vertexProvider) MatchesHost(host string) bool { return isVertexHost(host) }

// UpstreamFor picks the upstream: a pinned host, else the inbound host when it
// is already a Vertex host (transparent mode), else the host for the path's
// location (env-override mode, where the inbound host is the gateway).
func (p vertexProvider) UpstreamFor(r *http.Request) string {
	if p.host != "" {
		return p.host
	}
	if r == nil {
		return globalHost
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	if h := stripPort(host); isVertexHost(h) {
		return strings.ToLower(h)
	}
	if r.URL != nil {
		if rt, ok := parseRoute(r.URL.Path); ok {
			return hostFor(rt.location)
		}
	}
	return globalHost
}

// Matches recognizes a Vertex project/location path in env-override mode. It
// must be registered ahead of gemini and anthropic, whose looser heuristics
// (":generateContent", the anthropic-version header) would otherwise claim
// Vertex traffic and forward it to the wrong upstream.
func (vertexProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	_, ok := parseRoute(r.URL.Path)
	return ok
}

// ParseRequest delegates to the route's wire parser, then applies what only
// the path carries (model, stream method).
func (vertexProvider) ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	rt := routeOf(r)
	switch rt.wire {
	case wireGemini:
		// gemini's path regex matches Vertex's ".../models/{m}:method" too.
		return geminiP.ParseRequest(r, body)
	case wireAnthropic:
		info := anthropicP.ParseRequest(r, body)
		info.Model = rt.model
		info.Stream = info.Stream || rt.method == "streamRawPredict"
		return info
	case wireOpenAI:
		info := openaiwire.ParseRequest(r, body)
		if info.Model == "" {
			info.Model = rt.model
		}
		return info
	default:
		// Not inference: report the path model but no input estimate, so the
		// request is recorded at $0 instead of billed on a guessed prompt size.
		return provider.RequestInfo{Model: rt.model}
	}
}

// NewStreamParser is the request-less fallback: it cannot know the wire, so it
// feeds every parser and keeps the first authoritative result.
func (vertexProvider) NewStreamParser(contentType string) provider.StreamParser {
	return newSniffParser(contentType)
}

// NewStreamParserFor selects the parser from the request's route.
func (vertexProvider) NewStreamParserFor(r *http.Request, contentType string) provider.StreamParser {
	switch routeOf(r).wire {
	case wireGemini:
		return geminiP.NewStreamParser(contentType)
	case wireAnthropic:
		return anthropicP.NewStreamParser(contentType)
	case wireOpenAI:
		return openaiwire.NewParser(contentType)
	default:
		return nopParser{}
	}
}

// Price prices by model family. The layered price sheet is consulted first
// (provider "vertex"); this is the last-resort fallback.
func (vertexProvider) Price(model string, inTok, outTok int) float64 {
	m := strings.ToLower(model)
	switch {
	case m == "" || m == "count-tokens":
		return 0
	case strings.HasPrefix(m, "claude"):
		return anthropicP.Price(model, inTok, outTok)
	default:
		return geminiP.Price(model, inTok, outTok)
	}
}

// ErrorResponse renders a Google API error envelope; a 402 budget block maps
// to FAILED_PRECONDITION. Vertex itself wraps every publisher's errors this way.
func (vertexProvider) ErrorResponse(status int, message string) (string, []byte) {
	return geminiP.ErrorResponse(status, message)
}

// RewriteModel down-scopes in the path for publisher routes (where the model
// lives) and in the body for the OpenAI-compatible endpoint.
func (vertexProvider) RewriteModel(r *http.Request, body []byte, newModel string) ([]byte, error) {
	rt := routeOf(r)
	if rt.model == "" {
		return openaiwire.RewriteModel(body, newModel)
	}
	// gemini's rewrite replaces the "models/{m}:" segment, which is the same
	// shape on every Vertex publisher path.
	out, err := geminiP.RewriteModel(r, body, newModel)
	if err != nil || rt.wire != wireOpenAI || !bodyHasModel(body) {
		return out, err
	}
	// OpenAI-wire MaaS rawPredict (e.g. Mistral) also names the model in the
	// body; leaving it stale would ask Vertex for two different models.
	return openaiwire.RewriteModel(out, newModel)
}

func bodyHasModel(body []byte) bool {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return false
	}
	_, ok := obj["model"]
	return ok
}

func routeOf(r *http.Request) route {
	if r == nil || r.URL == nil {
		return route{}
	}
	rt, _ := parseRoute(r.URL.Path)
	return rt
}

// stripPort removes a trailing ":port" from a non-IPv6 host.
func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i != -1 && !strings.ContainsRune(host[i+1:], ':') {
		return host[:i]
	}
	return host
}

// nopParser meters non-inference calls as zero usage.
type nopParser struct{}

func (nopParser) Write(b []byte) (int, error) { return len(b), nil }
func (nopParser) Finish() provider.Usage      { return provider.Usage{Approximated: true} }

// sniffParser tees into every wire parser and returns the first authoritative
// usage. Anthropic is tried before OpenAI because openaiwire also accepts
// input_tokens/output_tokens and would misread an Anthropic body.
type sniffParser struct {
	parsers []provider.StreamParser
}

func newSniffParser(contentType string) *sniffParser {
	return &sniffParser{parsers: []provider.StreamParser{
		anthropicP.NewStreamParser(contentType),
		geminiP.NewStreamParser(contentType),
		openaiwire.NewParser(contentType),
	}}
}

func (s *sniffParser) Write(b []byte) (int, error) {
	for _, p := range s.parsers {
		_, _ = p.Write(b)
	}
	return len(b), nil
}

func (s *sniffParser) Finish() provider.Usage {
	var fallback provider.Usage
	for i, p := range s.parsers {
		u := p.Finish()
		if !u.Approximated {
			return u
		}
		if i == 0 {
			fallback = u
		}
	}
	return fallback
}
