package openai_test

import (
	"io"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/provider/openai"
)

var _ = Describe("StreamParser", func() {
	It("reports authoritative usage from a final SSE usage chunk", func() {
		const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"

		p := openai.New().NewStreamParser("text/event-stream")

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
		Expect(u.InputTokens).To(Equal(11))
		Expect(u.OutputTokens).To(Equal(7))
	})

	It("approximates output tokens from content length when no usage is sent", func() {
		const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"12345678\"}}]}\n\n" +
			"data: [DONE]\n\n"

		p := openai.New().NewStreamParser("text/event-stream")
		_, err := io.Copy(p, strings.NewReader(stream))
		Expect(err).NotTo(HaveOccurred())

		u := p.Finish()
		Expect(u.Approximated).To(BeTrue())
		Expect(u.OutputTokens).To(Equal(2)) // 8 chars / 4
	})

	It("skips malformed JSON chunks without corrupting accounting", func() {
		const stream = "data: {not valid json}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: [DONE]\n\n"

		p := openai.New().NewStreamParser("text/event-stream")
		_, err := io.WriteString(p, stream)
		Expect(err).NotTo(HaveOccurred())

		u := p.Finish()
		Expect(u.OutputTokens).To(Equal(0)) // 2 chars / 4 == 0
		Expect(u.Approximated).To(BeTrue())
	})

	It("surfaces usage from a buffered (non-stream) JSON response", func() {
		const body = "{\"choices\":[{\"message\":{\"content\":\"hi\"}}]," +
			"\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":5}}"

		p := openai.New().NewStreamParser("application/json")
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
		body := []byte(`{"model":"gpt-4o-mini","stream":true,` +
			`"messages":[{"role":"user","content":"12345678"}]}`)
		info := openai.New().ParseRequest(body)
		Expect(info.Model).To(Equal("gpt-4o-mini"))
		Expect(info.Stream).To(BeTrue())
		Expect(info.ApproxInputTokens).To(Equal(2)) // 8 / 4
	})

	It("returns a zero RequestInfo for malformed JSON", func() {
		info := openai.New().ParseRequest([]byte("{broken"))
		Expect(info.Model).To(BeEmpty())
		Expect(info.ApproxInputTokens).To(Equal(0))
		Expect(info.Stream).To(BeFalse())
	})
})

var _ = Describe("Price", func() {
	It("prices known and unknown models by substring", func() {
		p := openai.New()
		// gpt-4o-mini: 0.15 + 0.60 = 0.75 for 1M in + 1M out.
		Expect(p.Price("gpt-4o-mini", 1_000_000, 1_000_000)).To(Equal(0.75))
		// gpt-4o: 2.50 + 10.0 = 12.5.
		Expect(p.Price("gpt-4o", 1_000_000, 1_000_000)).To(Equal(12.5))
		// unknown -> default 2.50 + 10.0.
		Expect(p.Price("o1-preview", 1_000_000, 1_000_000)).To(Equal(12.5))
	})
})
