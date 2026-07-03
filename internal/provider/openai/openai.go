// Package openai implements the provider.Provider abstraction for OpenAI's
// inference API (chat completions and responses endpoints).
//
// It recognizes OpenAI traffic by host or request path, extracts a heuristic
// input-token estimate from the request body, incrementally parses the streamed
// (SSE) or buffered (JSON) response to meter token usage, and prices the result
// using approximate spike defaults. All parsing tolerates malformed JSON and
// never panics on bad input.
package openai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
)

// provider name and upstream host constants.
const (
	name         = "openai"
	upstreamHost = "api.openai.com"
)

// openaiProvider is the concrete provider.Provider implementation for OpenAI.
type openaiProvider struct{}

// New returns a provider.Provider for OpenAI.
func New() provider.Provider {
	return openaiProvider{}
}

// Name returns the stable provider identifier.
func (openaiProvider) Name() string { return name }

// UpstreamHost returns the real OpenAI API host.
func (openaiProvider) UpstreamHost() string { return upstreamHost }

// Hosts returns the hostnames that identify OpenAI traffic for Host-first
// detection.
func (openaiProvider) Hosts() []string { return []string{upstreamHost} }

// Matches reports whether the request path looks like an OpenAI inference
// endpoint (chat completions or responses).
func (openaiProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	p := r.URL.Path
	return strings.HasSuffix(p, "/chat/completions") ||
		strings.HasSuffix(p, "/responses") ||
		p == "/v1/chat/completions"
}

// chatRequest is the subset of an OpenAI chat request we care about.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// chatMessage is a single message in a chat request. Content may be a plain
// string or an array of typed content parts, so it is decoded lazily.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ParseRequest extracts request metadata from the raw request body. Malformed
// JSON yields a zero-value RequestInfo rather than an error.
func (openaiProvider) ParseRequest(body []byte) provider.RequestInfo {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return provider.RequestInfo{}
	}

	var textLen int
	for _, m := range req.Messages {
		textLen += contentTextLen(m.Content)
	}

	return provider.RequestInfo{
		Model:             req.Model,
		ApproxInputTokens: textLen / 4,
		Stream:            req.Stream,
	}
}

// contentTextLen returns the number of text characters in a message content
// field, tolerating either a bare string or an array of content parts.
func contentTextLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return len(s)
	}

	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var n int
		for _, p := range parts {
			n += len(p.Text)
		}
		return n
	}

	return 0
}

// NewStreamParser returns a StreamParser for the given response Content-Type.
// Server-sent event streams and buffered JSON bodies are both handled; the
// distinction only affects how bytes are scanned in Write.
func (openaiProvider) NewStreamParser(contentType string) provider.StreamParser {
	return &streamParser{
		sse: strings.Contains(strings.ToLower(contentType), "text/event-stream"),
	}
}

// usagePayload mirrors the OpenAI "usage" object present on non-stream
// responses and on the final chunk when stream_options.include_usage is set.
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// chunk is the subset of a streamed or buffered response object we parse.
type chunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *usagePayload `json:"usage"`
}

// streamParser accumulates token usage from an OpenAI response as its bytes
// stream by. It buffers partial lines for SSE and the whole body for JSON.
type streamParser struct {
	sse bool

	buf     bytes.Buffer
	chars   int
	usage   usagePayload
	haveUse bool
}

// Write consumes response bytes. For SSE it processes complete lines as they
// arrive, keeping cheap per-write work; for JSON it accumulates the body to
// parse once at Finish. It never returns an error so tee'ing is not disrupted.
func (p *streamParser) Write(b []byte) (int, error) {
	n := len(b)
	if !p.sse {
		p.buf.Write(b)
		return n, nil
	}

	p.buf.Write(b)
	for {
		line, err := p.buf.ReadBytes('\n')
		if err != nil {
			// No complete line yet; retain the partial line for next Write.
			p.buf.Write(line)
			break
		}
		p.processLine(bytes.TrimRight(line, "\r\n"))
	}
	return n, nil
}

// processLine handles a single SSE line, extracting content deltas and any
// terminal usage payload from "data:" events.
func (p *streamParser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}

	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return // Never panic on malformed JSON; skip the chunk.
	}
	for _, ch := range c.Choices {
		p.chars += len(ch.Delta.Content)
	}
	if c.Usage != nil {
		p.usage = *c.Usage
		p.haveUse = true
	}
}

// Finish is called once at EOF and returns the accumulated usage. It prefers
// authoritative usage reported by the upstream; otherwise it approximates
// output tokens from accumulated content length.
func (p *streamParser) Finish() provider.Usage {
	if !p.sse {
		// Buffered JSON body: parse the whole payload once.
		var c chunk
		if err := json.Unmarshal(p.buf.Bytes(), &c); err == nil && c.Usage != nil {
			p.usage = *c.Usage
			p.haveUse = true
		}
	}

	if p.haveUse {
		return provider.Usage{
			InputTokens:  p.usage.PromptTokens,
			OutputTokens: p.usage.CompletionTokens,
			Approximated: false,
		}
	}

	return provider.Usage{
		OutputTokens: p.chars / 4,
		Approximated: true,
	}
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
