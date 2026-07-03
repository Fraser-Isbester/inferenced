// Package anthropic implements the provider.Provider abstraction for
// Anthropic's Messages API.
//
// It recognizes Anthropic traffic by host or request path, extracts a heuristic
// input-token estimate from the request body, incrementally parses the streamed
// (SSE) or buffered (JSON) response to meter token usage, and prices the result
// using approximate spike defaults. All parsing tolerates malformed JSON and
// never panics on bad input.
package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/reqmeta"
)

// provider name and upstream host constants.
const (
	name         = "anthropic"
	upstreamHost = "api.anthropic.com"
)

// anthropicProvider is the concrete provider.Provider implementation for
// Anthropic.
type anthropicProvider struct{}

// New returns a provider.Provider for Anthropic.
func New() provider.Provider {
	return anthropicProvider{}
}

// Name returns the stable provider identifier.
func (anthropicProvider) Name() string { return name }

// UpstreamHost returns the real Anthropic API host.
func (anthropicProvider) UpstreamHost() string { return upstreamHost }

// UpstreamScheme returns the scheme used to reach Anthropic.
func (anthropicProvider) UpstreamScheme() string { return "https" }

// Hosts returns the hostnames that identify Anthropic traffic for Host-first
// detection.
func (anthropicProvider) Hosts() []string { return []string{upstreamHost} }

// Matches reports whether the request path or headers look like an Anthropic
// inference endpoint.
func (anthropicProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	// The anthropic-version header is a reliable signal even when the path is
	// non-standard.
	if r.Header.Get("anthropic-version") != "" {
		return true
	}
	return strings.HasSuffix(r.URL.Path, "/v1/messages")
}

// messagesRequest is the subset of an Anthropic Messages request we care about.
type messagesRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []anthMessage   `json:"messages"`
	Stream   bool            `json:"stream"`
	Tools    []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// anthMessage is a single message. Content may be a plain string or an array of
// typed content blocks, so it is decoded lazily.
type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ParseRequest extracts request metadata from the raw request body. Malformed
// JSON yields a zero-value RequestInfo rather than an error. Anthropic carries
// the model in the body, so the request is unused.
func (anthropicProvider) ParseRequest(_ *http.Request, body []byte) provider.RequestInfo {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return provider.RequestInfo{}
	}

	textLen := contentTextLen(req.System)
	for _, m := range req.Messages {
		textLen += contentTextLen(m.Content)
	}

	return provider.RequestInfo{
		Model:             req.Model,
		ApproxInputTokens: textLen / 4,
		Stream:            req.Stream,
		Metadata:          extractMetadata(req),
	}
}

// toolUseBlock is the shape of a tool_use content block; Input is decoded lazily
// so we can pull file/command signals without imposing a schema on every tool.
type toolUseBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// extractMetadata builds the bounded metadata blob from an Anthropic request:
// advertised tools, tool-call counts, files touched, shell commands, and cwd/git
// context parsed from the system prompt. Returns nil when nothing is captured.
func extractMetadata(req messagesRequest) map[string]any {
	b := reqmeta.New()
	b.SetMessageCount(len(req.Messages))
	for _, t := range req.Tools {
		b.AddTool(t.Name)
	}
	for _, m := range req.Messages {
		var blocks []toolUseBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			continue // string content or non-array; no tool calls to mine
		}
		for _, blk := range blocks {
			if blk.Type != "tool_use" {
				continue
			}
			b.AddToolCall(blk.Name)
			addToolInputSignals(b, blk.Input)
		}
	}
	cwd, gitRepo, gitBranch := parseSystemEnv(req.System)
	b.SetEnv(cwd, gitRepo, gitBranch)
	return b.Map()
}

// addToolInputSignals pulls file paths and shell commands out of a tool_use
// input object, tolerating any schema.
func addToolInputSignals(b *reqmeta.Builder, input json.RawMessage) {
	if len(input) == 0 {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(input, &obj); err != nil {
		return
	}
	for _, k := range []string{"file_path", "path", "notebook_path"} {
		if s, ok := obj[k].(string); ok {
			b.AddFile(s)
		}
	}
	if s, ok := obj["command"].(string); ok {
		b.AddCommand(s)
	}
}

var (
	reCwd    = regexp.MustCompile(`(?i)working directory:\s*(.+)`)
	reGitYes = regexp.MustCompile(`(?i)is directory a git repo:\s*(yes|true)`)
	reBranch = regexp.MustCompile(`(?i)current branch:\s*(\S+)`)
)

// parseSystemEnv best-effort extracts working directory, whether it is a git
// repo, and the branch from Claude Code's system-prompt environment preamble.
// Unmatched fields are returned empty.
func parseSystemEnv(system json.RawMessage) (cwd, gitRepo, gitBranch string) {
	text := systemText(system)
	if text == "" {
		return "", "", ""
	}
	if m := reCwd.FindStringSubmatch(text); m != nil {
		cwd = strings.TrimSpace(m[1])
	}
	if reGitYes.MatchString(text) && cwd != "" {
		gitRepo = cwd // the repo is the working directory when it is a git repo
	}
	if m := reBranch.FindStringSubmatch(text); m != nil {
		gitBranch = m[1]
	}
	return cwd, gitRepo, gitBranch
}

// systemText flattens the system field (string or array of text blocks) to a
// single string for env parsing.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
			sb.WriteByte('\n')
		}
		return sb.String()
	}
	return ""
}

// contentTextLen returns the number of text characters in a content field,
// tolerating either a bare string or an array of typed content blocks.
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
func (anthropicProvider) NewStreamParser(contentType string) provider.StreamParser {
	return &streamParser{
		sse: strings.Contains(strings.ToLower(contentType), "text/event-stream"),
	}
}

// streamEvent is the subset of an Anthropic SSE data payload we parse. The
// Messages stream reports input_tokens on the message_start event's message
// usage and cumulative output_tokens on the message_delta event's usage.
type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *usagePayload `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
	Usage *usagePayload `json:"usage"`
}

// usagePayload mirrors the Anthropic "usage" object.
type usagePayload struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// streamParser accumulates token usage from an Anthropic response as its bytes
// stream by. It buffers partial lines for SSE and the whole body for JSON.
type streamParser struct {
	sse bool

	buf     bytes.Buffer
	chars   int
	inTok   int
	outTok  int
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

// processLine handles a single SSE line, extracting content deltas and usage
// payloads from "data:" events.
func (p *streamParser) processLine(line []byte) {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[len("data:"):])
	if len(data) == 0 {
		return
	}
	p.processData(data)
}

// processData decodes a single SSE data payload and folds any usage or text
// delta it carries into the accumulated accounting.
func (p *streamParser) processData(data []byte) {
	var e streamEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return // Never panic on malformed JSON; skip the event.
	}

	// message_start carries the authoritative input token count (and an initial
	// output token count) in message.usage.
	if e.Message != nil && e.Message.Usage != nil {
		if e.Message.Usage.InputTokens > 0 {
			p.inTok = e.Message.Usage.InputTokens
			p.haveUse = true
		}
		if e.Message.Usage.OutputTokens > 0 {
			p.outTok = e.Message.Usage.OutputTokens
			p.haveUse = true
		}
	}

	// message_delta carries the cumulative output token count in usage.
	if e.Usage != nil {
		if e.Usage.InputTokens > 0 {
			p.inTok = e.Usage.InputTokens
			p.haveUse = true
		}
		if e.Usage.OutputTokens > 0 {
			p.outTok = e.Usage.OutputTokens
			p.haveUse = true
		}
	}

	if e.Delta != nil {
		p.chars += len(e.Delta.Text)
	}
}

// Finish is called once at EOF and returns the accumulated usage. It prefers
// authoritative usage reported by the upstream; otherwise it approximates
// output tokens from accumulated content length.
func (p *streamParser) Finish() provider.Usage {
	if !p.sse {
		// Buffered JSON body: parse the whole payload once. A non-stream
		// Messages response carries a top-level "usage" object.
		var e streamEvent
		if err := json.Unmarshal(p.buf.Bytes(), &e); err == nil && e.Usage != nil {
			p.inTok = e.Usage.InputTokens
			p.outTok = e.Usage.OutputTokens
			p.haveUse = true
		}
	}

	if p.haveUse {
		return provider.Usage{
			InputTokens:  p.inTok,
			OutputTokens: p.outTok,
			Approximated: false,
		}
	}

	return provider.Usage{
		OutputTokens: p.chars / 4,
		Approximated: true,
	}
}

// price is the per-1M-token cost in USD for a model family. These are
// approximate spike defaults, not a source of truth for billing.
type price struct {
	inPerM, outPerM float64
}

var (
	priceSonnet  = price{inPerM: 3.0, outPerM: 15.0}
	priceOpus    = price{inPerM: 15.0, outPerM: 75.0}
	priceHaiku   = price{inPerM: 0.80, outPerM: 4.0}
	priceDefault = price{inPerM: 3.0, outPerM: 15.0}
)

// Price returns the approximate cost in USD for the given model and token
// counts, matching the model family by substring.
// RewriteModel sets the JSON body "model" field to newModel for down-scoping.
func (anthropicProvider) RewriteModel(_ *http.Request, body []byte, newModel string) ([]byte, error) {
	return provider.RewriteBodyModel(body, newModel)
}

func (anthropicProvider) Price(model string, inTok, outTok int) float64 {
	m := strings.ToLower(model)
	pr := priceDefault
	switch {
	case strings.Contains(m, "haiku"):
		pr = priceHaiku
	case strings.Contains(m, "opus"):
		pr = priceOpus
	case strings.Contains(m, "sonnet"):
		pr = priceSonnet
	}
	return float64(inTok)/1e6*pr.inPerM + float64(outTok)/1e6*pr.outPerM
}

// ErrorResponse renders Anthropic's native error envelope, mapping the HTTP
// status to Anthropic's error type. A budget block (402) maps to
// "invalid_request_error" — the same terminal, non-retriable, non-auth type
// Anthropic returns for "credit balance too low".
func (anthropicProvider) ErrorResponse(status int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrType(status),
			"message": message,
		},
	})
	return "application/json", body
}

// anthropicErrType maps an HTTP status to Anthropic's native error type string.
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}
