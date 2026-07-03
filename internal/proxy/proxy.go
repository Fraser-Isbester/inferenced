// Package proxy implements the provider-agnostic reverse proxy that intercepts
// LLM inference traffic, forwards it upstream over HTTPS, tees the streamed
// response to meter token usage without buffering the body, and emits a
// structured telemetry Event once the response completes.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/identity"
	"github.com/Fraser-Isbester/inferenced/internal/logx"
	"github.com/Fraser-Isbester/inferenced/internal/pricing"
	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// ctxKey is a private context key type so the Director can stash per-request
// state for ModifyResponse without colliding with other packages' keys.
type ctxKey int

const (
	// eventKey stores the partially populated *telemetry.Event.
	eventKey ctxKey = iota
	// providerKey stores the detected provider.Provider.
	providerKey
	// startKey stores the request start time.Time.
	startKey
)

// reqCounter is a per-process monotonic counter used to mint request IDs.
var reqCounter atomic.Uint64

// Options carries per-deployment attribution overrides. A non-empty DeveloperID
// replaces the anonymized credential hash; Team is attached to every event.
type Options struct {
	DeveloperID string
	Team        string
	// Pricing resolves per-model cost from the layered price sheets. When nil,
	// or when it cannot price a model, the provider's built-in Price is used.
	Pricing *pricing.Resolver
}

// New returns a reverse proxy that detects the target provider, forwards the
// request upstream (scheme per provider), and tees the response body so token
// usage and cost can be metered and emitted to sink. The returned value is an
// *httputil.ReverseProxy so tests may override its Transport.
func New(reg *provider.Registry, sink telemetry.Sink, opts Options) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{}

	// FlushInterval -1 flushes writes to the client immediately, which is
	// required for low-latency Server-Sent Events streaming.
	rp.FlushInterval = -1
	rp.Transport = &http.Transport{ForceAttemptHTTP2: true}

	// priceOf prefers the layered price resolver and falls back to the
	// provider's built-in price when the resolver has no entry for the model.
	priceOf := func(p provider.Provider, model string, inTok, outTok int) float64 {
		if opts.Pricing != nil {
			if cost, ok := opts.Pricing.Price(p.Name(), model, inTok, outTok); ok {
				return cost
			}
		}
		return p.Price(model, inTok, outTok)
	}

	rp.Rewrite = func(pr *httputil.ProxyRequest) {
		out := pr.Out
		p, ok := reg.Detect(pr.In)
		if !ok {
			// Unknown traffic: best-effort passthrough to the request's own host
			// so the proxy does not fail on a relative outbound URL.
			out.URL.Scheme = "https"
			if out.URL.Host == "" {
				out.URL.Host = pr.In.Host
			}
			out.Host = out.URL.Host
			return
		}

		host := p.UpstreamHost()
		out.URL.Scheme = p.UpstreamScheme()
		out.URL.Host = host
		// Set both so the upstream sees the correct Host header.
		out.Host = host

		// Strip the client's Accept-Encoding. When the inbound request carries
		// it, Go's Transport forwards it verbatim and does NOT auto-decompress
		// the response, so the tee'd observer copy would be gzip/br bytes the SSE
		// parser cannot read — yielding OutputTokens=0. Removing it lets the
		// Transport advertise gzip and transparently decompress, so the observer
		// (and the client) see plain text. Bandwidth is irrelevant on localhost.
		out.Header.Del("Accept-Encoding")

		// Read and restore the body so ParseRequest can inspect it while the
		// forwarded request still carries a replayable body.
		var body []byte
		if out.Body != nil {
			b, err := io.ReadAll(out.Body)
			_ = out.Body.Close()
			body = b // keep whatever was read, even on a partial read
			if err != nil {
				// Surface the read failure rather than silently forwarding a
				// truncated/empty inference request.
				fmt.Printf("[WARN]     --> request body read error: %v\n", err)
			}
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		out.ContentLength = int64(len(body))

		info := p.ParseRequest(out, body)

		cred := identity.Credential(out)
		devID := opts.DeveloperID
		if devID == "" {
			devID = identity.DeveloperID(cred)
		}
		reqID := fmt.Sprintf("%d", reqCounter.Add(1))
		start := time.Now()

		ev := &telemetry.Event{
			RequestID:       reqID,
			TimestampUnixMs: start.UnixMilli(),
			Provider:        p.Name(),
			DeveloperID:     devID,
			Team:            opts.Team,
			OSUser:          identity.OSUser(),
			Method:          out.Method,
			Path:            out.URL.Path,
			Model:           info.Model,
			Stream:          info.Stream,
			InputTokens:     info.ApproxInputTokens,
		}

		logx.Request(reqID, out.Method, out.URL.Path, p.Name(), info.Model, devID, logx.Redact(cred))

		ctx := out.Context()
		ctx = context.WithValue(ctx, eventKey, ev)
		ctx = context.WithValue(ctx, providerKey, p)
		ctx = context.WithValue(ctx, startKey, start)
		pr.Out = out.WithContext(ctx)
	}

	rp.ModifyResponse = func(resp *http.Response) error {
		ev, ok := resp.Request.Context().Value(eventKey).(*telemetry.Event)
		if !ok || ev == nil {
			return nil
		}
		p, ok := resp.Request.Context().Value(providerKey).(provider.Provider)
		if !ok || p == nil {
			return nil
		}
		start, _ := resp.Request.Context().Value(startKey).(time.Time)

		ev.Status = resp.StatusCode
		logx.StreamStart(ev.RequestID)

		parser := p.NewStreamParser(resp.Header.Get("Content-Type"))

		// io.Pipe is unbuffered: the observer goroutine below must continuously
		// drain pr or writes to pw (driven by the client read path via TeeReader)
		// will block, stalling the client. Keep parser.Write cheap.
		pr, pw := io.Pipe()
		model := ev.Model

		go func() {
			// Drain the tee'd bytes into the parser. io.Copy returns when the
			// writer closes pw (normal EOF) or the pipe is broken.
			_, _ = io.Copy(parser, pr)

			u := parser.Finish()
			// Prefer the parser's authoritative input count; otherwise keep the
			// Director's request-body estimate (already on ev.InputTokens). This
			// matters for e.g. OpenAI streams sent without stream_options.include_usage,
			// where the parser reports InputTokens=0 but a usable estimate exists.
			if u.InputTokens > 0 {
				ev.InputTokens = u.InputTokens
			} else {
				ev.TokensApproximated = true
			}
			ev.OutputTokens = u.OutputTokens
			if u.Approximated {
				ev.TokensApproximated = true
			}
			ev.CostUSD = priceOf(p, model, ev.InputTokens, ev.OutputTokens)
			ev.DurationMs = time.Since(start).Milliseconds()
			sink.Emit(*ev)

			// Release any pending writer; further reads see EOF.
			_ = pr.Close()
		}()

		resp.Body = &teeBody{
			r:      io.TeeReader(resp.Body, pw),
			upBody: resp.Body,
			pw:     pw,
		}
		return nil
	}

	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		fmt.Printf("[ERROR]    <-- upstream error: %v\n", err)
		// Upstream transport failures never reach ModifyResponse, so meter the
		// failed request here instead of losing it. Only one of ErrorHandler or
		// ModifyResponse runs per request, so this stays exactly-once.
		if ev, ok := req.Context().Value(eventKey).(*telemetry.Event); ok && ev != nil {
			ev.Status = http.StatusBadGateway
			if start, ok := req.Context().Value(startKey).(time.Time); ok {
				ev.DurationMs = time.Since(start).Milliseconds()
			}
			if p, ok := req.Context().Value(providerKey).(provider.Provider); ok && p != nil {
				ev.CostUSD = priceOf(p, ev.Model, ev.InputTokens, ev.OutputTokens)
			}
			ev.TokensApproximated = true
			sink.Emit(*ev)
		}
		w.WriteHeader(http.StatusBadGateway)
	}

	return rp
}

// teeBody wraps the upstream response body. Reads pass through an io.TeeReader
// so every byte delivered to the client is also mirrored to the observer pipe.
// Close closes BOTH the upstream body and the pipe writer, which is essential:
// closing pw unblocks the observer goroutine and lets it see EOF, so an early
// client disconnect (ReverseProxy closes resp.Body) cannot leak the goroutine
// or hang.
type teeBody struct {
	r      io.Reader
	upBody io.ReadCloser
	pw     *io.PipeWriter
}

// Read reads from the tee'd stream, mirroring bytes to the observer pipe.
func (t *teeBody) Read(p []byte) (int, error) {
	return t.r.Read(p)
}

// Close closes both the upstream body and the pipe writer so the observer
// goroutine unblocks and emits exactly once.
func (t *teeBody) Close() error {
	err := t.upBody.Close()
	// Closing the writer signals EOF to the observer's io.Copy(parser, pr).
	_ = t.pw.Close()
	return err
}
