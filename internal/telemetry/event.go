// Package telemetry defines the structured Event emitted for each intercepted
// inference request and the Sink abstraction that records it.
package telemetry

import (
	"fmt"
	"sync"
)

// Event is the structured record emitted once an intercepted inference request
// completes. It captures request identity, provider attribution, token usage,
// cost, and timing.
type Event struct {
	// RequestID is a per-process monotonic identifier for the request.
	RequestID string
	// TimestampUnixMs is the request start time in Unix milliseconds.
	TimestampUnixMs int64
	// Provider is the detected provider name.
	Provider string
	// DeveloperID is the developer identity: a configured id when set, otherwise
	// the anonymized hash derived from the credential.
	DeveloperID string
	// Team is the optional team attribution from config.
	Team string
	// OSUser is the local operating-system username.
	OSUser string
	// Method is the HTTP request method.
	Method string
	// Path is the HTTP request path.
	Path string
	// Model is the requested model identifier.
	Model string
	// Stream reports whether the client requested a streamed response.
	Stream bool
	// Status is the upstream HTTP response status code.
	Status int
	// InputTokens is the number of input tokens.
	InputTokens int
	// OutputTokens is the number of output tokens.
	OutputTokens int
	// TokensApproximated is true when token counts were estimated.
	TokensApproximated bool
	// CostUSD is the computed cost in US dollars.
	CostUSD float64
	// DurationMs is the total request duration in milliseconds.
	DurationMs int64
}

// Sink records the final [RESPONSE] Event. Implementations must be safe for
// concurrent use.
type Sink interface {
	// Emit renders or records the final [RESPONSE] event.
	Emit(Event)
}

// StdoutSink is a Sink that prints a human-readable summary line to stdout. It
// guards concurrent Emit calls with a mutex.
type StdoutSink struct {
	mu sync.Mutex
}

// NewStdoutSink returns a ready-to-use StdoutSink.
func NewStdoutSink() *StdoutSink {
	return &StdoutSink{}
}

// Emit prints a single [RESPONSE] summary line for the event. Concurrent calls
// are serialized so their output does not interleave.
func (s *StdoutSink) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	approx := ""
	if e.TokensApproximated {
		approx = " (approx)"
	}

	fmt.Printf("[RESPONSE] <-- Complete | Status: %d | Provider: %s | Model: %s | Tokens In: ~%d | Tokens Out: ~%d | Approx Cost: $%.5f%s\n",
		e.Status, e.Provider, e.Model, e.InputTokens, e.OutputTokens, e.CostUSD, approx)
}
