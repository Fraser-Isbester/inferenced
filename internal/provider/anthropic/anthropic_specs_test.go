package anthropic_test

import (
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
)

var _ = Describe("StreamParser", func() {
	It("reports authoritative usage from message_start and message_delta", func() {
		const stream = "event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"

		p := anthropic.New().NewStreamParser("text/event-stream")

		// Write in small slices to exercise partial-line buffering across writes.
		r := strings.NewReader(stream)
		buf := make([]byte, 8)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				_, werr := p.Write(buf[:n])
				Expect(werr).NotTo(HaveOccurred())
			}
			if err == io.EOF {
				break
			}
			Expect(err).NotTo(HaveOccurred())
		}

		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(10))
		Expect(u.OutputTokens).To(Equal(5))
	})

	It("approximates output tokens from delta text when no usage is present", func() {
		const stream = "event: content_block_delta\n" +
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"12345678"}}` + "\n\n"

		p := anthropic.New().NewStreamParser("text/event-stream")
		_, err := io.Copy(p, strings.NewReader(stream))
		Expect(err).NotTo(HaveOccurred())

		u := p.Finish()
		Expect(u.Approximated).To(BeTrue())
		Expect(u.OutputTokens).To(Equal(2)) // 8 chars / 4
	})

	It("surfaces usage from a buffered (non-stream) JSON response", func() {
		const body = `{"usage":{"input_tokens":20,"output_tokens":5}}`

		p := anthropic.New().NewStreamParser("application/json")
		_, err := io.WriteString(p, body)
		Expect(err).NotTo(HaveOccurred())

		u := p.Finish()
		Expect(u.Approximated).To(BeFalse())
		Expect(u.InputTokens).To(Equal(20))
		Expect(u.OutputTokens).To(Equal(5))
	})
})

var _ = Describe("ParseRequest", func() {
	It("extracts model, stream flag, and approximate input tokens", func() {
		body := []byte(`{"model":"claude-3-5-sonnet","stream":true,` +
			`"messages":[{"role":"user","content":"12345678"}]}`)
		info := anthropic.New().ParseRequest(nil, body)
		Expect(info.Model).To(Equal("claude-3-5-sonnet"))
		Expect(info.Stream).To(BeTrue())
		Expect(info.ApproxInputTokens).To(Equal(2)) // 8 / 4
	})

	It("returns a zero RequestInfo for malformed JSON", func() {
		info := anthropic.New().ParseRequest(nil, []byte("{broken"))
		Expect(info.Model).To(BeEmpty())
		Expect(info.ApproxInputTokens).To(Equal(0))
		Expect(info.Stream).To(BeFalse())
	})
})

var _ = Describe("Price", func() {
	It("prices model families by substring", func() {
		p := anthropic.New()
		// sonnet: 3 + 15 = 18 for 1M in + 1M out.
		Expect(p.Price("claude-3-5-sonnet", 1_000_000, 1_000_000)).To(Equal(18.0))
		// opus: 15 + 75 = 90.
		Expect(p.Price("claude-3-opus", 1_000_000, 1_000_000)).To(Equal(90.0))
		// haiku: 0.80 + 4 = 4.8.
		Expect(p.Price("claude-3-5-haiku", 1_000_000, 1_000_000)).To(Equal(4.8))
		// unknown -> default 3 + 15 = 18.
		Expect(p.Price("claude-x", 1_000_000, 1_000_000)).To(Equal(18.0))
	})
})
