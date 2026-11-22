// Package identity extracts and anonymizes the caller identity from an
// inference request, so telemetry can attribute usage without logging secrets.
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os/user"
	"strings"
)

// Credential returns the API credential carried by the request, checking the
// auth locations used across providers in priority order: "x-api-key"
// (Anthropic), "Authorization: Bearer" (OpenAI and compatibles), "api-key"
// (Azure OpenAI), "x-goog-api-key" (Gemini), then the "key" query parameter
// (Gemini). A SigV4 Authorization (Bedrock) yields its access-key ID, since
// the rest of the header changes with every request; a provider that must
// rank it first implements provider.CredentialSource. It returns "" when none
// is present (e.g. a local Ollama call).
func Credential(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		if akid, ok := SigV4AccessKey(v); ok {
			return akid
		}
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	if v := r.Header.Get("api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("x-goog-api-key"); v != "" {
		return v
	}
	if r.URL != nil {
		if v := r.URL.Query().Get("key"); v != "" {
			return v
		}
	}
	return ""
}

// SigV4AccessKey returns the access-key ID from an
// "AWS4-HMAC-SHA256 Credential=AKID/date/region/service/aws4_request, ..." value.
func SigV4AccessKey(auth string) (string, bool) {
	rest, ok := strings.CutPrefix(auth, "AWS4-HMAC-SHA256 ")
	if !ok {
		return "", false
	}
	for _, part := range strings.Split(rest, ",") {
		scope, ok := strings.CutPrefix(strings.TrimSpace(part), "Credential=")
		if !ok {
			continue
		}
		akid, _, _ := strings.Cut(scope, "/")
		return akid, akid != ""
	}
	return "", false
}

// DeveloperID returns a stable, anonymized identifier for a credential. An empty
// credential yields "anonymous"; otherwise it returns the first 12 hex
// characters of the SHA-256 of the credential.
func DeveloperID(credential string) string {
	if credential == "" {
		return "anonymous"
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])[:12]
}

// Client returns a friendly name for the coding agent / client that made the
// request, classified from the User-Agent header (e.g. "Claude Code", "Cursor").
// Unknown agents fall back to the leading User-Agent product token; an absent
// header yields "".
func Client(r *http.Request) string {
	ua := strings.ToLower(r.Header.Get("User-Agent"))
	switch {
	case ua == "":
		return ""
	case strings.Contains(ua, "claude-cli"), strings.Contains(ua, "claude-code"):
		return "Claude Code"
	case strings.Contains(ua, "cursor"):
		return "Cursor"
	case strings.Contains(ua, "aider"):
		return "aider"
	case strings.Contains(ua, "copilot"):
		return "Copilot"
	case strings.Contains(ua, "cline"):
		return "Cline"
	case strings.Contains(ua, "windsurf"), strings.Contains(ua, "codeium"):
		return "Windsurf"
	case strings.Contains(ua, "openai"):
		return "OpenAI SDK"
	case strings.Contains(ua, "anthropic"):
		return "Anthropic SDK"
	case strings.Contains(ua, "genai"), strings.Contains(ua, "google-api"):
		return "Gemini SDK"
	default:
		// Fall back to the leading product token, e.g. "curl/8.1" -> "curl".
		token := ua
		if i := strings.IndexAny(token, "/ "); i > 0 {
			token = token[:i]
		}
		return token
	}
}

// OSUser returns the local operating-system username, or "unknown" if it cannot
// be determined.
func OSUser() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return u.Username
}
