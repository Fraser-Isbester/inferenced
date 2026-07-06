package identity_test

import (
	"net/http/httptest"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/identity"
)

func TestIdentity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Identity Suite")
}

var _ = Describe("Client", func() {
	classify := func(ua string) string {
		r := httptest.NewRequest("POST", "/", nil)
		if ua != "" {
			r.Header.Set("User-Agent", ua)
		}
		return identity.Client(r)
	}

	It("classifies known coding agents", func() {
		Expect(classify("claude-cli/1.2.3 (external, cli)")).To(Equal("Claude Code"))
		Expect(classify("Cursor/0.42")).To(Equal("Cursor"))
		Expect(classify("openai-python/1.30")).To(Equal("OpenAI SDK"))
	})

	It("falls back to the leading product token for unknown agents", func() {
		Expect(classify("curl/8.1.2")).To(Equal("curl"))
	})

	It("returns empty when no User-Agent is present", func() {
		Expect(classify("")).To(Equal(""))
	})
})

var _ = Describe("Credential", func() {
	It("prefers x-api-key", func() {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("x-api-key", "sk-ant-123")
		r.Header.Set("Authorization", "Bearer other")
		Expect(identity.Credential(r)).To(Equal("sk-ant-123"))
	})

	It("strips the Bearer prefix from Authorization", func() {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Authorization", "Bearer sk-openai-xyz")
		Expect(identity.Credential(r)).To(Equal("sk-openai-xyz"))
	})

	It("reads the Azure api-key header", func() {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("api-key", "azure-key")
		Expect(identity.Credential(r)).To(Equal("azure-key"))
	})

	It("reads the Gemini x-goog-api-key header", func() {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("x-goog-api-key", "goog-key")
		Expect(identity.Credential(r)).To(Equal("goog-key"))
	})

	It("reads the Gemini key query parameter", func() {
		r := httptest.NewRequest("POST", "/v1beta/models/x:generateContent?key=query-key", nil)
		Expect(identity.Credential(r)).To(Equal("query-key"))
	})

	It("returns empty when no credential is present", func() {
		r := httptest.NewRequest("POST", "/api/chat", nil)
		Expect(identity.Credential(r)).To(BeEmpty())
	})
})

var _ = Describe("DeveloperID", func() {
	It("is anonymous for an empty credential", func() {
		Expect(identity.DeveloperID("")).To(Equal("anonymous"))
	})

	It("is a stable 12-char hash otherwise", func() {
		id := identity.DeveloperID("sk-ant-123")
		Expect(id).To(HaveLen(12))
		Expect(id).To(Equal(identity.DeveloperID("sk-ant-123")))
		Expect(id).NotTo(Equal(identity.DeveloperID("sk-ant-456")))
	})
})
