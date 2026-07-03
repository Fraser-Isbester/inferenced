// Package gemini implements the provider.Provider abstraction for Google's
// Generative Language API (Gemini).
//
// Gemini carries the model in the URL path ("models/<model>:generateContent")
// and reports usage in a "usageMetadata" object. Streaming responses arrive
// either as an SSE stream ("?alt=sse") or as a chunked JSON array; the parser
// buffers its observation copy and extracts the last usageMetadata at Finish,
// which leaves the client hot path unaffected.
package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
)

const (
	name         = "gemini"
	upstreamHost = "generativelanguage.googleapis.com"
)

// geminiProvider is the concrete provider.Provider implementation for Gemini.
type geminiProvider struct{}

// New returns a provider.Provider for Gemini.
func New() provider.Provider { return geminiProvider{} }

// Name returns the stable provider identifier.
func (geminiProvider) Name() string { return name }

// UpstreamHost returns the real Gemini API host.
func (geminiProvider) UpstreamHost() string { return upstreamHost }

// UpstreamScheme returns the scheme used to reach Gemini.
func (geminiProvider) UpstreamScheme() string { return "https" }

// Hosts returns the hostnames that identify Gemini traffic for Host-first
// detection.
func (geminiProvider) Hosts() []string { return []string{upstreamHost} }

// modelRe extracts the model from a Gemini path such as
// "/v1beta/models/gemini-1.5-flash:streamGenerateContent".
var modelRe = regexp.MustCompile(`models/([^:/?]+):`)

// Matches reports whether the request looks like a Gemini generateContent call
// or carries Gemini's API-key header.
func (geminiProvider) Matches(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	if r.Header.Get("x-goog-api-key") != "" {
		return true
	}
	p := r.URL.Path
	return strings.Contains(p, ":generateContent") ||
		strings.Contains(p, ":streamGenerateContent")
}

// genRequest is the subset of a Gemini request body we inspect for input size.
type genRequest struct {
	Contents []struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
}

// ParseRequest reads the model from the URL path and estimates input tokens from
// the request contents.
func (geminiProvider) ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	var model string
	stream := false
	if r != nil && r.URL != nil {
		if m := modelRe.FindStringSubmatch(r.URL.Path); m != nil {
			model = m[1]
		}
		stream = strings.Contains(r.URL.Path, ":streamGenerateContent")
	}

	var req genRequest
	_ = json.Unmarshal(body, &req) // tolerate malformed JSON
	var textLen int
	for _, c := range req.Contents {
		for _, part := range c.Parts {
			textLen += len(part.Text)
		}
	}

	return provider.RequestInfo{
		Model:             model,
		ApproxInputTokens: textLen / 4,
		Stream:            stream,
	}
}

// NewStreamParser returns a Gemini usage parser. Both SSE and JSON-array
// streams are handled by buffering and scanning for usageMetadata at Finish.
func (geminiProvider) NewStreamParser(string) provider.StreamParser {
	return &streamParser{}
}

// usageMetadata mirrors Gemini's token accounting object.
type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// streamParser accumulates the response bytes and extracts the final
// usageMetadata at Finish. Buffering here only affects the observation copy.
type streamParser struct {
	buf bytes.Buffer
}

// Write buffers the tee'd bytes. It never errors so tee'ing is not disrupted.
func (p *streamParser) Write(b []byte) (int, error) {
	return p.buf.Write(b)
}

// Finish returns the usage parsed from the last usageMetadata object, falling
// back to a character-based estimate of output tokens when none is present.
func (p *streamParser) Finish() provider.Usage {
	if raw, ok := lastUsageMetadata(p.buf.Bytes()); ok {
		var um usageMetadata
		if err := json.Unmarshal(raw, &um); err == nil {
			out := um.CandidatesTokenCount
			if out == 0 && um.TotalTokenCount > um.PromptTokenCount {
				out = um.TotalTokenCount - um.PromptTokenCount
			}
			return provider.Usage{
				InputTokens:  um.PromptTokenCount,
				OutputTokens: out,
				Approximated: false,
			}
		}
	}

	// Fallback: approximate output tokens from any "text" fields in the body.
	return provider.Usage{
		OutputTokens: approxOutputTokens(p.buf.Bytes()),
		Approximated: true,
	}
}

// usageMetaKey locates each "usageMetadata": occurrence.
var usageMetaKey = regexp.MustCompile(`"usageMetadata"\s*:\s*`)

// lastUsageMetadata returns the balanced-brace JSON object following the last
// "usageMetadata": key (usage is cumulative, so the last one wins). Balanced
// extraction tolerates nested fields such as promptTokensDetails.
func lastUsageMetadata(b []byte) ([]byte, bool) {
	locs := usageMetaKey.FindAllIndex(b, -1)
	if len(locs) == 0 {
		return nil, false
	}
	start := locs[len(locs)-1][1]
	if start >= len(b) || b[start] != '{' {
		return nil, false
	}
	depth := 0
	for i := start; i < len(b); i++ {
		switch b[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : i+1], true
			}
		}
	}
	return nil, false
}

// textRe extracts the value of each "text" field for the fallback estimate.
var textRe = regexp.MustCompile(`"text"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// approxOutputTokens estimates output tokens as accumulated text length / 4.
func approxOutputTokens(b []byte) int {
	var n int
	for _, m := range textRe.FindAllSubmatch(b, -1) {
		n += len(m[1])
	}
	return n / 4
}

// Price returns the approximate cost in USD, matching the model family by
// substring. Values are approximate spike defaults per 1M tokens.
type price struct {
	inPerM, outPerM float64
}

var (
	pricePro     = price{inPerM: 1.25, outPerM: 5.0}
	priceFlash   = price{inPerM: 0.075, outPerM: 0.30}
	priceDefault = price{inPerM: 1.25, outPerM: 5.0}
)

// Price prices Gemini pro/flash families by substring.
func (geminiProvider) Price(model string, inTok, outTok int) float64 {
	m := strings.ToLower(model)
	pr := priceDefault
	switch {
	case strings.Contains(m, "flash"):
		pr = priceFlash
	case strings.Contains(m, "pro"):
		pr = pricePro
	}
	return float64(inTok)/1e6*pr.inPerM + float64(outTok)/1e6*pr.outPerM
}
