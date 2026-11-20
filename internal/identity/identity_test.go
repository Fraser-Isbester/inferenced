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

	It("keys a SigV4 Authorization by its access-key ID, not the per-request signature", func() {
		a := httptest.NewRequest("POST", "/model/x/invoke", nil)
		a.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261120/us-east-1/bedrock/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=aaaa")
		b := httptest.NewRequest("POST", "/model/x/invoke", nil)
		b.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261121/us-west-2/bedrock/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date, Signature=bbbb")
		Expect(identity.Credential(a)).To(Equal("AKIDEXAMPLE"))
		Expect(identity.Credential(b)).To(Equal(identity.Credential(a)))
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
