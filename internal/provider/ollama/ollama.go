// Package ollama implements the provider.Provider abstraction for a local
// Ollama runtime.
//
// Ollama serves plain HTTP on localhost:11434 and streams newline-delimited
// JSON (one object per line, no "data:" framing). The final object (done:true)
// carries prompt_eval_count and eval_count. Because it runs locally, cost is $0
// but token usage is still metered.
package ollama

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
)

const name = "ollama"

// defaultUpstream is the standard local Ollama address.
const defaultUpstream = "localhost:11434"

// ollamaProvider is the concrete provider.Provider implementation for Ollama.
type ollamaProvider struct {
	upstream string
}

// New returns a provider.Provider for a local Ollama runtime at the default
// address.
func New() provider.Provider { return ollamaProvider{upstream: defaultUpstream} }

// NewWithUpstream returns a provider for an Ollama runtime at a custom
// host:port (used by config).
func NewWithUpstream(upstream string) provider.Provider {
	if upstream == "" {
		upstream = defaultUpstream
	}
	return ollamaProvider{upstream: upstream}
}

// Name returns the stable provider identifier.
func (ollamaProvider) Name() string { return name }

// UpstreamHost returns the configured Ollama host:port.
func (p ollamaProvider) UpstreamHost() string { return p.upstream }

// UpstreamScheme returns "http": Ollama serves plain HTTP locally.
func (ollamaProvider) UpstreamScheme() string { return "http" }

// Hosts returns the addresses that identify Ollama traffic for Host-first
// detection.
func (p ollamaProvider) Hosts() []string {
	return []string{p.upstream, "localhost:11434", "127.0.0.1:11434"}
}

// Matches reports whether the request path looks like an Ollama API call.
func (ollamaProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := r.URL.Path
	return strings.HasSuffix(p, "/api/chat") ||
		strings.HasSuffix(p, "/api/generate")
}

// genRequest is the subset of an Ollama request body we inspect.
type genRequest struct {
	Model    string `json:"model"`
	Prompt   string `json:"prompt"`
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}

// ParseRequest reads the model from the body and estimates input tokens from the
// prompt or chat messages.
func (ollamaProvider) ParseRequest(_ *http.Request, body []byte) provider.RequestInfo {
	var req genRequest
	_ = json.Unmarshal(body, &req) // tolerate malformed JSON

	textLen := len(req.Prompt)
	for _, m := range req.Messages {
		textLen += len(m.Content)
	}

	return provider.RequestInfo{
		Model:             req.Model,
		ApproxInputTokens: textLen / 4,
		// Ollama streams by default; the response is NDJSON regardless.
		Stream: true,
	}
}

// NewStreamParser returns an NDJSON usage parser.
func (ollamaProvider) NewStreamParser(string) provider.StreamParser {
	return &streamParser{}
}

// ndjsonObject is the subset of an Ollama response object we parse. The counts
// appear on the final object (done:true).
type ndjsonObject struct {
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// streamParser accumulates the latest token counts seen across NDJSON lines.
type streamParser struct {
	buf     bytes.Buffer
	in      int
	out     int
	haveUse bool
}

// Write processes complete newline-delimited JSON objects as they arrive.
func (p *streamParser) Write(b []byte) (int, error) {
	n := len(b)
	p.buf.Write(b)
	for {
		line, err := p.buf.ReadBytes('\n')
		if err != nil {
			p.buf.Write(line) // retain the partial line for the next Write
			break
		}
		p.processLine(bytes.TrimSpace(line))
	}
	return n, nil
}

// processLine decodes one NDJSON object and records any token counts it carries.
func (p *streamParser) processLine(line []byte) {
	if len(line) == 0 {
		return
	}
	var o ndjsonObject
	if err := json.Unmarshal(line, &o); err != nil {
		return // Never panic on malformed JSON; skip the line.
	}
	if o.PromptEvalCount > 0 || o.EvalCount > 0 {
		p.in = o.PromptEvalCount
		p.out = o.EvalCount
		p.haveUse = true
	}
}

// Finish parses any trailing buffered object and returns the accumulated usage.
func (p *streamParser) Finish() provider.Usage {
	if rem := bytes.TrimSpace(p.buf.Bytes()); len(rem) > 0 {
		p.processLine(rem)
	}
	return provider.Usage{
		InputTokens:  p.in,
		OutputTokens: p.out,
		Approximated: !p.haveUse,
	}
}

// Price returns 0: Ollama runs locally at no per-token cost.
func (ollamaProvider) Price(string, int, int) float64 { return 0 }

// RewriteModel sets the JSON body "model" field to newModel for down-scoping.
func (ollamaProvider) RewriteModel(_ *http.Request, body []byte, newModel string) ([]byte, error) {
	return provider.RewriteBodyModel(body, newModel)
}

// ErrorResponse renders Ollama's native error envelope, a flat object with a
// single "error" string. The status is unused.
func (ollamaProvider) ErrorResponse(_ int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string]any{
		"error": message,
	})
	return "application/json", body
}
