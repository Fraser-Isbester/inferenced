// Package logx provides small, dependency-free logging helpers for the proxy's
// human-readable request/stream lines and for redacting secrets.
package logx

import "fmt"

// hintLen is the number of leading credential characters preserved as a hint
// before the redaction marker.
const hintLen = 6

// Redact returns a safe-to-log form of a credential: a short leading hint
// followed by "[Redacted]", e.g. "sk-ant...[Redacted]". An empty credential
// returns "[none]".
func Redact(credential string) string {
	if credential == "" {
		return "[none]"
	}
	hint := credential
	if len(hint) > hintLen {
		hint = hint[:hintLen]
	}
	return hint + "...[Redacted]"
}

// Request prints the [REQUEST] line describing an intercepted inference request.
// reqID may be used by callers to correlate lines.
func Request(reqID, method, path, provider, model, devID, redactedAuth string) {
	fmt.Printf("[REQUEST]  --> %s %s | Provider: %s | Model: %s | Dev: %s | Auth: %s\n",
		method, path, provider, model, devID, redactedAuth)
}

// StreamStart prints the [STREAM] line indicating that response stream chunks
// are being processed. reqID may be used by callers to correlate lines.
func StreamStart(reqID string) {
	fmt.Printf("[STREAM]   --> Processing stream chunks...\n")
}
