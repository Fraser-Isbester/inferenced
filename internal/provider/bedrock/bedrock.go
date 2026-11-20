// Package bedrock implements the provider.Provider abstraction for AWS Bedrock
// Runtime, metering only.
//
// Bedrock requests are SigV4-signed: the signature covers the Host header, the
// path, the signed headers and a hash of the body. Two consequences shape this
// provider:
//
//   - Env-override routing cannot work. An SDK pointed at the gateway signs for
//     the gateway's host, and forwarding to bedrock-runtime.{region}.amazonaws.com
//     then fails signature checks. So Matches never claims traffic: Bedrock is
//     detected by Host only, which requires transparent interception, where the
//     client signs for the real host and the request is forwarded byte-for-byte.
//   - Nothing may change the request. RewriteModel refuses, so a downgrade rule
//     forwards the request unchanged; enforcement can still block it (a refusal
//     needs no signature).
//
// The model is in the path (/model/{modelId}/invoke and friends). Streams use
// AWS event-stream framing, decoded here; the payloads inside are the model's
// native wire (Anthropic Messages for Claude) or the Converse API's events.
package bedrock

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/Fraser-Isbester/inferenced/internal/identity"
	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
)

const (
	name          = "bedrock"
	defaultRegion = "us-east-1"
	// eventStreamType is the Content-Type of InvokeModelWithResponseStream and
	// ConverseStream responses.
	eventStreamType = "application/vnd.amazon.eventstream"
)

// ErrRewriteUnsupported is returned by RewriteModel: a signed request can't be
// changed without re-signing it, which inferenced does not do.
var ErrRewriteUnsupported = errors.New("bedrock: model rewrite unsupported on SigV4-signed requests")

var (
	// hostRe is the Bedrock Runtime host family, FIPS endpoints included. The
	// region is constrained because a matching inbound host is dialed verbatim.
	hostRe = regexp.MustCompile(`^bedrock-runtime(?:-fips)?\.[a-z0-9-]+\.amazonaws\.com$`)
	// routeRe matches the escaped path so a model ARN's encoded slashes stay in
	// one segment.
	routeRe = regexp.MustCompile(`^/model/([^/]+)/(invoke|invoke-with-response-stream|converse|converse-stream)$`)
	// regionRe validates a configured region before it becomes a hostname.
	regionRe = regexp.MustCompile(`^[a-z0-9-]+$`)
)

var anthropicP = anthropic.New()

type bedrockProvider struct {
	region string
}

// New returns a Bedrock provider whose fallback upstream is us-east-1.
func New() provider.Provider { return bedrockProvider{region: defaultRegion} }

// NewWithRegion returns a Bedrock provider whose fallback upstream is region.
// An empty or malformed region falls back to us-east-1.
func NewWithRegion(region string) provider.Provider {
	if !regionRe.MatchString(region) {
		region = defaultRegion
	}
	return bedrockProvider{region: region}
}

// Name returns the stable provider identifier.
func (bedrockProvider) Name() string { return name }

// UpstreamHost returns the configured region's host. Requests normally go to
// their own inbound host via UpstreamFor.
func (p bedrockProvider) UpstreamHost() string {
	return "bedrock-runtime." + p.region + ".amazonaws.com"
}

// UpstreamScheme returns the scheme used to reach Bedrock.
func (bedrockProvider) UpstreamScheme() string { return "https" }

// Hosts is empty: the regional host family is matched by MatchesHost.
func (bedrockProvider) Hosts() []string { return nil }

// MatchesHost claims bedrock-runtime.{region}.amazonaws.com.
func (bedrockProvider) MatchesHost(host string) bool {
	return hostRe.MatchString(strings.ToLower(host))
}

// UpstreamFor forwards to the inbound host exactly as received: the client
// signed that value, port and case included, so any other would fail the
// signature. Only the normalized form is validated.
func (p bedrockProvider) UpstreamFor(r *http.Request) string {
	if r == nil {
		return ""
	}
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}
	if hostRe.MatchString(strings.ToLower(stripPort(host))) {
		return host
	}
	return p.UpstreamHost()
}

// Credential is the SigV4 access key: it is what AWS authenticates, while any
// other credential header could be added unsigned to pick another identity.
func (bedrockProvider) Credential(r *http.Request) string {
	if akid, ok := identity.SigV4AccessKey(r.Header.Get("Authorization")); ok {
		return akid
	}
	return identity.Credential(r)
}

// Matches never claims traffic; see the package comment.
func (bedrockProvider) Matches(*http.Request) bool { return false }

// route is what the path says about a request.
type route struct {
	model string
	op    string
}

func routeOf(r *http.Request) (route, bool) {
	if r == nil || r.URL == nil {
		return route{}, false
	}
	m := routeRe.FindStringSubmatch(r.URL.EscapedPath())
	if m == nil {
		return route{}, false
	}
	model, err := url.PathUnescape(m[1])
	if err != nil {
		model = m[1]
	}
	// An inference-profile or provisioned-model ARN: keep the id after the last
	// slash, which is what pricing matches on and leaves the account out.
	if i := strings.LastIndexByte(model, '/'); i != -1 {
		model = model[i+1:]
	}
	return route{model: model, op: m[2]}, true
}

// ParseRequest reads the model from the path. InvokeModel bodies for Claude are
// Anthropic Messages, so they get the Anthropic parser's estimate and metadata.
func (bedrockProvider) ParseRequest(r *http.Request, body []byte) provider.RequestInfo {
	rt, ok := routeOf(r)
	if !ok {
		// Not an inference call (count-tokens, say): nothing billable.
		return provider.RequestInfo{}
	}
	info := provider.RequestInfo{Model: rt.model, Stream: strings.HasSuffix(rt.op, "stream")}
	switch {
	case strings.HasPrefix(rt.op, "converse"):
		info.ApproxInputTokens = converseTextLen(body) / 4
	case strings.Contains(strings.ToLower(rt.model), "anthropic.") || strings.Contains(strings.ToLower(rt.model), "claude"):
		ai := anthropicP.ParseRequest(r, body)
		info.ApproxInputTokens = ai.ApproxInputTokens
		if info.ApproxInputTokens == 0 {
			// Legacy Claude Text Completions carry a "prompt", not messages.
			info.ApproxInputTokens = promptLen(body) / 4
		}
		info.Metadata = ai.Metadata
		info.Artifacts = ai.Artifacts
	default:
		info.ApproxInputTokens = len(body) / 4
	}
	return info
}

// promptLen is the length of a Text Completions request's prompt.
func promptLen(body []byte) int {
	var req struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &req)
	return len(req.Prompt)
}

// converseTextLen sums the text of a Converse request's system and messages.
func converseTextLen(body []byte) int {
	type block struct {
		Text string `json:"text"`
	}
	var req struct {
		System   []block `json:"system"`
		Messages []struct {
			Content []block `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return len(body)
	}
	n := 0
	for _, b := range req.System {
		n += len(b.Text)
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			n += len(b.Text)
		}
	}
	return n
}

// NewStreamParser picks event-stream decoding or a buffered JSON body by
// Content-Type. Either way the payloads are folded by the same usage rules, so
// the route need not be known.
func (bedrockProvider) NewStreamParser(contentType string) provider.StreamParser {
	return &parser{stream: strings.HasPrefix(strings.ToLower(contentType), eventStreamType)}
}

// Price prices by model family; the layered price sheet ("bedrock") is
// consulted first.
func (bedrockProvider) Price(model string, inTok, outTok int) float64 {
	m := strings.ToLower(model)
	if strings.Contains(m, "claude") {
		return anthropicP.Price(model, inTok, outTok)
	}
	pr := priceDefault
	for _, f := range families {
		if strings.Contains(m, f.match) {
			pr = f.price
			break
		}
	}
	return float64(inTok)/1e6*pr.inPerM + float64(outTok)/1e6*pr.outPerM
}

type price struct{ inPerM, outPerM float64 }

// Approximate list prices (us-east-1, on demand); the remote catalog overrides
// them. Unknown models price like Sonnet so a budget errs toward blocking.
var (
	priceDefault = price{3, 15}
	families     = []struct {
		match string
		price price
	}{
		{"nova-micro", price{0.035, 0.14}},
		{"nova-lite", price{0.06, 0.24}},
		{"nova-pro", price{0.8, 3.2}},
	}
)

// ErrorResponse renders the AWS JSON error shape. SDKs read the error type
// from "__type"; a budget block (402) maps to AccessDeniedException, which
// they treat as terminal and non-retriable.
func (bedrockProvider) ErrorResponse(status int, message string) (string, []byte) {
	body, _ := json.Marshal(map[string]string{"__type": errType(status), "message": message})
	return "application/json", body
}

func errType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "ThrottlingException"
	case status == http.StatusBadRequest:
		return "ValidationException"
	case status == http.StatusServiceUnavailable:
		return "ServiceUnavailableException"
	case status >= 500:
		return "InternalServerException"
	default:
		return "AccessDeniedException"
	}
}

// RewriteModel refuses and returns the body unchanged; applyDowngrade then
// forwards the request as signed.
func (bedrockProvider) RewriteModel(_ *http.Request, body []byte, _ string) ([]byte, error) {
	return body, ErrRewriteUnsupported
}

// parser meters a Bedrock response: an event stream decoded frame by frame, or
// a JSON body parsed once at Finish.
type parser struct {
	stream bool
	buf    []byte
	broken bool
	acc    usage
}

// maxFrame bounds one event-stream message; AWS caps them at 16 MiB.
const maxFrame = 16 << 20

func (p *parser) Write(b []byte) (int, error) {
	if p.broken {
		return len(b), nil
	}
	p.buf = append(p.buf, b...)
	if p.stream {
		p.drain()
	}
	return len(b), nil
}

// drain decodes every complete frame in buf. A frame that fails its length or
// CRC checks stops decoding: framing is lost, and what was counted so far
// stands.
func (p *parser) drain() {
	off := 0
	defer func() {
		// Compact once per Write, not per frame: a Write carrying many frames
		// would otherwise copy the remainder for each one.
		if p.buf != nil && off > 0 {
			p.buf = append([]byte(nil), p.buf[off:]...)
		}
	}()
	for b := p.buf; len(b)-off >= 12; {
		f := b[off:]
		total := binary.BigEndian.Uint32(f[0:4])
		hlen := binary.BigEndian.Uint32(f[4:8])
		if total < 16 || total > maxFrame || hlen > total-16 ||
			crc32.ChecksumIEEE(f[0:8]) != binary.BigEndian.Uint32(f[8:12]) {
			p.broken, p.buf = true, nil
			return
		}
		if uint32(len(f)) < total {
			return
		}
		msg := f[:total]
		if crc32.ChecksumIEEE(msg[:total-4]) != binary.BigEndian.Uint32(msg[total-4:]) {
			p.broken, p.buf = true, nil
			return
		}
		headers := parseHeaders(msg[12 : 12+hlen])
		if headers[":message-type"] == "event" {
			p.acc.fold(msg[12+hlen : total-4])
		}
		off += int(total)
	}
}

// parseHeaders returns an event-stream message's string headers.
func parseHeaders(b []byte) map[string]string {
	out := map[string]string{}
	for len(b) > 0 {
		n := int(b[0])
		if len(b) < 1+n+1 {
			return out
		}
		key := string(b[1 : 1+n])
		typ := b[1+n]
		b = b[2+n:]
		var size int
		switch typ {
		case 0, 1: // bool true / false: no value
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7: // bytes, string: 2-byte length prefix
			if len(b) < 2 {
				return out
			}
			l := int(binary.BigEndian.Uint16(b))
			if len(b) < 2+l {
				return out
			}
			if typ == 7 {
				out[key] = string(b[2 : 2+l])
			}
			b = b[2+l:]
			continue
		default:
			return out
		}
		if len(b) < size {
			return out
		}
		b = b[size:]
	}
	return out
}

func (p *parser) Finish() provider.Usage {
	if !p.stream && !p.broken {
		p.acc.fold(p.buf)
	}
	return p.acc.result()
}

// usage folds whatever token counts a payload carries. Bedrock's own
// invocation metrics are authoritative and final; before they arrive, the
// model wire's counts are used.
type usage struct {
	in, out int
	have    bool
	final   bool
	chars   int
	// provisional: out so far is only Anthropic message_start's placeholder,
	// so a stream cut there under-reports what was generated.
	provisional bool
}

type counts struct {
	InputTokens  int `json:"inputTokens"`   // Converse
	OutputTokens int `json:"outputTokens"`  // Converse
	InputSnake   int `json:"input_tokens"`  // Anthropic
	OutputSnake  int `json:"output_tokens"` // Anthropic
}

type payload struct {
	// Bytes wraps an InvokeModelWithResponseStream chunk: the model's own
	// event, base64 in the JSON and decoded by encoding/json.
	Bytes   []byte `json:"bytes"`
	Metrics *struct {
		In  int `json:"inputTokenCount"`
		Out int `json:"outputTokenCount"`
	} `json:"amazon-bedrock-invocationMetrics"`
	Usage   *counts `json:"usage"`
	Message *struct {
		Usage *counts `json:"usage"`
	} `json:"message"` // Anthropic message_start
	PromptTokens int `json:"prompt_token_count"`     // Llama
	GenTokens    int `json:"generation_token_count"` // Llama, cumulative
	Delta        *struct {
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"` // Anthropic tool_use input
		ToolUse     *struct {
			Input string `json:"input"`
		} `json:"toolUse"` // Converse tool_use input
	} `json:"delta"` // Anthropic content_block_delta, Converse contentBlockDelta
	Generation string `json:"generation"` // Llama
	// Native Nova InvokeModel streams wrap Converse-shaped events.
	ContentBlockDelta *payload `json:"contentBlockDelta"`
	Metadata          *payload `json:"metadata"`
	// Titan Text: inputTextTokenCount on the response or first chunk; output
	// as per-result counts (non-stream) or a running total (stream).
	TitanIn      int `json:"inputTextTokenCount"`
	TitanOutRun  int `json:"totalOutputTextTokenCount"`
	TitanResults []struct {
		TokenCount int    `json:"tokenCount"`
		OutputText string `json:"outputText"`
	} `json:"results"`
	OutputText string `json:"outputText"` // Titan stream
	Completion string `json:"completion"` // Claude Text Completions
}

func (u *usage) fold(b []byte) {
	var p payload
	if json.Unmarshal(bytes.TrimSpace(b), &p) != nil {
		return
	}
	u.foldPayload(p)
}

func (u *usage) foldPayload(p payload) {
	if len(p.Bytes) > 0 {
		u.fold(p.Bytes)
	}
	for _, w := range []*payload{p.ContentBlockDelta, p.Metadata} {
		if w != nil {
			u.foldPayload(*w)
		}
	}
	if p.Delta != nil {
		u.chars += len(p.Delta.Text) + len(p.Delta.PartialJSON)
		if p.Delta.ToolUse != nil {
			u.chars += len(p.Delta.ToolUse.Input)
		}
	}
	u.chars += len(p.Generation) + len(p.OutputText) + len(p.Completion)
	titanOut := p.TitanOutRun
	for _, r := range p.TitanResults {
		titanOut += r.TokenCount
		u.chars += len(r.OutputText)
	}
	if m := p.Metrics; m != nil {
		u.in, u.out, u.have, u.final = m.In, m.Out, true, true
		return
	}
	if u.final {
		return
	}
	if c := messageUsage(p); c != nil {
		u.set(max(c.InputTokens, c.InputSnake), max(c.OutputTokens, c.OutputSnake))
		u.provisional = true
	}
	if c := p.Usage; c != nil {
		u.setOut(max(c.InputTokens, c.InputSnake), max(c.OutputTokens, c.OutputSnake))
	}
	u.setOut(p.PromptTokens, p.GenTokens)
	u.setOut(p.TitanIn, titanOut)
}

// setOut records counts whose output, when present, is reported rather than
// a placeholder.
func (u *usage) setOut(in, out int) {
	u.set(in, out)
	if out > 0 {
		u.provisional = false
	}
}

func messageUsage(p payload) *counts {
	if p.Message == nil {
		return nil
	}
	return p.Message.Usage
}

// set records non-zero counts; streamed counts are cumulative, so a later
// value replaces an earlier one.
func (u *usage) set(in, out int) {
	if in > 0 {
		u.in, u.have = in, true
	}
	if out > 0 {
		u.out, u.have = out, true
	}
}

func (u *usage) result() provider.Usage {
	if u.have {
		// A stream cut before its final counts still produced the text seen
		// so far; don't bill less than that.
		if u.provisional && !u.final && u.chars/4 > u.out {
			return provider.Usage{InputTokens: u.in, OutputTokens: u.chars / 4, Approximated: true}
		}
		return provider.Usage{InputTokens: u.in, OutputTokens: u.out}
	}
	return provider.Usage{OutputTokens: u.chars / 4, Approximated: true}
}

// stripPort removes a trailing ":port" from a non-IPv6 host.
func stripPort(host string) string {
	if i := strings.LastIndexByte(host, ':'); i != -1 && !strings.ContainsRune(host[i+1:], ':') {
		return host[:i]
	}
	return host
}
