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

// Credential returns the API credential carried by the request. It prefers the
// "x-api-key" header, then falls back to the Authorization header with any
// leading "Bearer " prefix stripped, and returns "" when neither is present.
func Credential(r *http.Request) string {
	if v := r.Header.Get("x-api-key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return ""
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

// OSUser returns the local operating-system username, or "unknown" if it cannot
// be determined.
func OSUser() string {
	u, err := user.Current()
	if err != nil {
		return "unknown"
	}
	return u.Username
}
