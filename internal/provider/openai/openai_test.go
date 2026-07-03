package openai

import (
	"io"
	"strings"
	"testing"
)

// TestStreamParserSSEUsage feeds a canned OpenAI SSE stream (content deltas, a
// final chunk carrying usage, and the "[DONE]" sentinel) through the parser and
// asserts Finish reports the authoritative usage.
func TestStreamParserSSEUsage(t *testing.T) {
	const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	p := New().NewStreamParser("text/event-stream")

	// Write in small slices to exercise partial-line buffering across writes.
	r := strings.NewReader(stream)
	buf := make([]byte, 8)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := p.Write(buf[:n]); werr != nil {
				t.Fatalf("Write returned error: %v", werr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	u := p.Finish()
	if u.Approximated {
		t.Errorf("Approximated = true, want false (authoritative usage present)")
	}
	if u.InputTokens != 11 {
		t.Errorf("InputTokens = %d, want 11", u.InputTokens)
	}
	if u.OutputTokens != 7 {
		t.Errorf("OutputTokens = %d, want 7", u.OutputTokens)
	}
}

// TestStreamParserApproxWhenNoUsage verifies that when no usage payload is sent,
// output tokens are approximated from accumulated content length.
func TestStreamParserApproxWhenNoUsage(t *testing.T) {
	const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n\n" +
		"data: [DONE]\n\n"

	p := New().NewStreamParser("text/event-stream")
	if _, err := io.Copy(p, strings.NewReader(stream)); err != nil {
		t.Fatalf("copy: %v", err)
	}

	u := p.Finish()
	if !u.Approximated {
		t.Errorf("Approximated = false, want true")
	}
	if u.OutputTokens != 2 { // 8 chars / 4
		t.Errorf("OutputTokens = %d, want 2", u.OutputTokens)
	}
}

// TestStreamParserMalformedJSON ensures malformed chunks are skipped without
// panicking and do not corrupt accounting.
func TestStreamParserMalformedJSON(t *testing.T) {
	const stream = "data: {not valid json}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"

	p := New().NewStreamParser("text/event-stream")
	if _, err := io.WriteString(p, stream); err != nil {
		t.Fatalf("write: %v", err)
	}
	u := p.Finish()
	if u.OutputTokens != 0 { // 2 chars / 4 == 0
		t.Errorf("OutputTokens = %d, want 0", u.OutputTokens)
	}
	if !u.Approximated {
		t.Errorf("Approximated = false, want true")
	}
}

// TestNonStreamJSONUsage verifies buffered JSON responses surface usage.
func TestNonStreamJSONUsage(t *testing.T) {
	const body = "{\"choices\":[{\"message\":{\"content\":\"hi\"}}]," +
		"\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5}}"

	p := New().NewStreamParser("application/json")
	if _, err := io.WriteString(p, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	u := p.Finish()
	if u.Approximated {
		t.Errorf("Approximated = true, want false")
	}
	if u.InputTokens != 20 || u.OutputTokens != 5 {
		t.Errorf("usage = %d/%d, want 20/5", u.InputTokens, u.OutputTokens)
	}
}

// TestParseRequest checks model, stream flag, and approximate input tokens.
func TestParseRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-mini","stream":true,` +
		`"messages":[{"role":"user","content":"12345678"}]}`)
	info := New().ParseRequest(body)
	if info.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want gpt-4o-mini", info.Model)
	}
	if !info.Stream {
		t.Errorf("Stream = false, want true")
	}
	if info.ApproxInputTokens != 2 { // 8/4
		t.Errorf("ApproxInputTokens = %d, want 2", info.ApproxInputTokens)
	}
}

// TestParseRequestMalformed ensures bad JSON yields a zero RequestInfo.
func TestParseRequestMalformed(t *testing.T) {
	info := New().ParseRequest([]byte("{broken"))
	if info.Model != "" || info.ApproxInputTokens != 0 || info.Stream {
		t.Errorf("got %+v, want zero RequestInfo", info)
	}
}

// TestPrice checks substring-based model pricing.
func TestPrice(t *testing.T) {
	p := New()
	// 1M in + 1M out for gpt-4o-mini => 0.15 + 0.60 = 0.75.
	if got := p.Price("gpt-4o-mini", 1_000_000, 1_000_000); got != 0.75 {
		t.Errorf("gpt-4o-mini price = %v, want 0.75", got)
	}
	// gpt-4o => 2.50 + 10.0 = 12.5.
	if got := p.Price("gpt-4o", 1_000_000, 1_000_000); got != 12.5 {
		t.Errorf("gpt-4o price = %v, want 12.5", got)
	}
	// unknown => default 2.50 + 10.0.
	if got := p.Price("o1-preview", 1_000_000, 1_000_000); got != 12.5 {
		t.Errorf("default price = %v, want 12.5", got)
	}
}
