// Package openaiwire implements the OpenAI wire-format parsing shared by the
// first-party OpenAI provider and any config-defined OpenAI-compatible endpoint
// (Azure OpenAI, OpenRouter, Groq, Together, vLLM, LM Studio, ...).
//
// It handles the chat-completions, responses, and embeddings shapes for both
// streamed (SSE) and buffered (JSON) responses, and tolerates malformed JSON
// without panicking.
package openaiwire

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
)

// request is the subset of an OpenAI-style request body we inspect. Messages
// covers chat completions; Input covers the responses and embeddings endpoints.
type request struct {
	Model    string          `json:"model"`
	Messages []message       `json:"messages"`
	Input    json.RawMessage `json:"input"`
	Stream   bool            `json:"stream"`
}

// message is a single chat message. Content may be a plain string or an array of
// typed content parts, so it is decoded lazily.
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// deploymentRe extracts a model/deployment name from a URL path, covering both
// Azure ("/openai/deployments/<name>/...") and generic ("/models/<name>...")
// layouts, so attribution still works when the body omits "model".
var deploymentRe = regexp.MustCompile(`/(?:deployments|models)/([^/:?]+)`)

// ParseRequest extracts request metadata from the body, falling back to the URL
// path for the model when the body omits it. Malformed JSON yields a zero-value
// RequestInfo rather than an error.
// RewriteModel sets the top-level "model" field of an OpenAI-wire request body
// to newModel, shared by the first-party OpenAI provider and any
// OpenAI-compatible endpoint. Azure-style deployment-in-path routing is not
// rewritten (the body model is the wire default).
func RewriteModel(body []byte, newModel string) ([]byte, error) {
	return provider.RewriteBodyModel(body, newModel)
}

func ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	var req request
	_ = json.Unmarshal(body, &req) // tolerate malformed JSON; use whatever decoded

	textLen := 0
	for _, m := range req.Messages {
		textLen += contentTextLen(m.Content)
	}
	textLen += contentTextLen(req.Input)

	model := req.Model
	if model == "" && r != nil && r.URL != nil {
		if m := deploymentRe.FindStringSubmatch(r.URL.Path); m != nil {
			model = m[1]
		}
	}

	return provider.RequestInfo{
		Model:             model,
		ApproxInputTokens: textLen / 4,
		Stream:            req.Stream,
	}
}

// contentTextLen returns the number of text characters in a content field,
// tolerating a bare string, an array of strings, or an array of typed parts.
func contentTextLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return len(s)
	}

	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		var n int
		for _, s := range strs {
			n += len(s)
		}
		return n
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

// ErrType maps an HTTP status to an OpenAI-style error "type" string. A budget
// block (402) maps to "insufficient_quota" — OpenAI's terminal quota-exceeded
// type — so coding agents treat it as a billing stop rather than an auth
// failure or a retryable error.
func ErrType(status int) string {
	switch {
	case status == http.StatusPaymentRequired:
		return "insufficient_quota"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// NewParser returns a StreamParser for the given response Content-Type.
func NewParser(contentType string) provider.StreamParser {
	return &parser{
		sse: strings.Contains(strings.ToLower(contentType), "text/event-stream"),
	}
}

// usagePayload mirrors every OpenAI-style usage shape: chat completions
// (prompt/completion), responses (input/output), and embeddings (prompt/total).
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

func (u usagePayload) in() int {
	if u.PromptTokens > 0 {
		return u.PromptTokens
	}
	return u.InputTokens
}

func (u usagePayload) out() int {
	if u.CompletionTokens > 0 {
		return u.CompletionTokens
	}
	return u.OutputTokens
}

func (u usagePayload) any() bool {
	return u.PromptTokens > 0 || u.CompletionTokens > 0 ||
		u.InputTokens > 0 || u.OutputTokens > 0 || u.TotalTokens > 0
}

// chunk is the subset of a streamed or buffered response object we parse. It
// covers chat deltas (Choices[].Delta.Content), responses text deltas (Delta on
// a "response.output_text.delta" event), a top-level usage object, and the
// responses "completed" event carrying usage under "response".
type chunk struct {
	Type    string `json:"type"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Delta    string        `json:"delta"`
	Usage    *usagePayload `json:"usage"`
	Response *struct {
		Usage *usagePayload `json:"usage"`
	} `json:"response"`
}

// parser accumulates token usage from an OpenAI-style response as its bytes
// stream by. It processes complete SSE lines as they arrive and buffers a
// non-SSE body for a single parse at Finish.
type parser struct {
	sse bool

	buf     bytes.Buffer
	chars   int
	usage   usagePayload
	haveUse bool
}

// Write consumes response bytes. It never returns an error so tee'ing is not
// disrupted.
func (p *parser) Write(b []byte) (int, error) {
	n := len(b)
	if !p.sse {
		p.buf.Write(b)
		return n, nil
	}

	p.buf.Write(b)
	for {
		line, err := p.buf.ReadBytes('\n')
		if err != nil {
			// No complete line yet; retain the partial line for the next Write.
			p.buf.Write(line)
			break
		}
		p.processLine(bytes.TrimRight(line, "\r\n"))
	}
	return n, nil
}

// processLine handles a single SSE line, decoding "data:" payloads.
func (p *parser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	p.fold(data)
}

// fold decodes one payload and merges any content delta or usage it carries.
func (p *parser) fold(data []byte) {
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return // Never panic on malformed JSON; skip the payload.
	}
	for _, ch := range c.Choices {
		p.chars += len(ch.Delta.Content)
	}
	if c.Type == "response.output_text.delta" {
		p.chars += len(c.Delta)
	}
	if c.Usage != nil && c.Usage.any() {
		p.usage = *c.Usage
		p.haveUse = true
	}
	if c.Response != nil && c.Response.Usage != nil && c.Response.Usage.any() {
		p.usage = *c.Response.Usage
		p.haveUse = true
	}
}

// Finish returns the accumulated usage, preferring authoritative counts and
// otherwise approximating output tokens from accumulated content length.
func (p *parser) Finish() provider.Usage {
	if !p.sse {
		p.fold(p.buf.Bytes())
	}

	if p.haveUse {
		return provider.Usage{
			InputTokens:  p.usage.in(),
			OutputTokens: p.usage.out(),
			Approximated: false,
		}
	}

	return provider.Usage{
		OutputTokens: p.chars / 4,
		Approximated: true,
	}
}
