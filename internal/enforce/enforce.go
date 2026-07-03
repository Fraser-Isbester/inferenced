// Package enforce evaluates local spend limits against a pluggable spend
// source and, via HTTP middleware, blocks or warns on over-limit inference
// requests with a provider-shaped error.
//
// The engine is decoupled from persistence and configuration: callers inject a
// SpendFunc (typically backed by the local SQLite ledger) and a clock, so
// period math and limit evaluation are fully testable without a database.
package enforce

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
)

// Scope identifies which attribution dimension a limit applies to.
type Scope string

const (
	// ScopeTeam scopes a limit to the team attribution.
	ScopeTeam Scope = "team"
	// ScopeDeveloper scopes a limit to the developer attribution.
	ScopeDeveloper Scope = "developer"
)

// Period identifies the budget window a limit is measured over.
type Period string

const (
	// PeriodDay measures spend since the start of the current local day.
	PeriodDay Period = "day"
	// PeriodMonth measures spend since the first of the current local month.
	PeriodMonth Period = "month"
	// PeriodRolling measures spend over a trailing Window ending now.
	PeriodRolling Period = "rolling"
)

// Action selects what the middleware does when a limit is violated.
type Action string

const (
	// ActionBlock rejects the request with a 403 provider-shaped error.
	ActionBlock Action = "block"
	// ActionWarn logs a warning and forwards the request.
	ActionWarn Action = "warn"
)

// Limit describes a single spend ceiling for a scope/id over a period.
type Limit struct {
	// Scope is the attribution dimension (team or developer).
	Scope Scope
	// ID is the specific team or developer id the limit applies to; "*" matches
	// any id within the scope.
	ID string
	// Period is the budget window (day, month, or rolling).
	Period Period
	// Window is the trailing duration used when Period is PeriodRolling.
	Window time.Duration
	// LimitUSD is the spend ceiling in US dollars.
	LimitUSD float64
}

// SpendFunc returns the total spend in US dollars for the given scope and id
// since the given time. An id of "*" means "all ids within the scope".
type SpendFunc func(scope, id string, since time.Time) (float64, error)

// Decision is the result of evaluating the configured limits for a caller.
type Decision struct {
	// Allowed reports whether the request should proceed.
	Allowed bool
	// Limit is the violated limit when Allowed is false; nil otherwise.
	Limit *Limit
	// SpentUSD is the spend observed for the violated limit.
	SpentUSD float64
}

// ResolveFunc extracts the developer id and team attribution from a request.
type ResolveFunc func(*http.Request) (devID, team string)

// Engine evaluates a fixed set of limits against an injected spend source and
// clock. It is safe for concurrent use provided the SpendFunc is.
type Engine struct {
	limits []Limit
	spend  SpendFunc
	clock  func() time.Time
}

// NewEngine returns an Engine that evaluates the given limits using spend for
// spend lookups and clock for the current time. If clock is nil, time.Now is
// used.
func NewEngine(limits []Limit, spend SpendFunc, clock func() time.Time) *Engine {
	if clock == nil {
		clock = time.Now
	}
	return &Engine{limits: limits, spend: spend, clock: clock}
}

// Check evaluates every applicable limit for the given developer and team and
// returns the first violation. A limit applies when its scope matches and its
// id is "*" or equals the caller's id for that scope. A limit is violated when
// observed spend is greater than or equal to its ceiling. Spend-lookup errors
// fail open (the request is allowed) and are logged.
func (e *Engine) Check(devID, team string) Decision {
	now := e.clock()
	for i := range e.limits {
		l := e.limits[i]
		switch l.Scope {
		case ScopeDeveloper:
			if l.ID != "*" && l.ID != devID {
				continue
			}
		case ScopeTeam:
			if l.ID != "*" && l.ID != team {
				continue
			}
		default:
			// Unknown scope never applies.
			continue
		}

		start := periodStart(l, now)
		spent, err := e.spend(string(l.Scope), l.ID, start)
		if err != nil {
			// Fail open: never block traffic because the ledger query failed.
			log.Printf("[LIMIT] spend query error: scope=%s id=%q: %v", l.Scope, l.ID, err)
			continue
		}
		if spent >= l.LimitUSD {
			violated := l
			return Decision{Allowed: false, Limit: &violated, SpentUSD: spent}
		}
	}
	return Decision{Allowed: true}
}

// periodStart returns the inclusive lower bound of the limit's current budget
// window relative to now.
func periodStart(l Limit, now time.Time) time.Time {
	switch l.Period {
	case PeriodDay:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	case PeriodMonth:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	case PeriodRolling:
		return now.Add(-l.Window)
	default:
		// Unknown period behaves like a rolling window of zero: since==now.
		return now
	}
}

// Middleware returns an HTTP middleware that enforces the engine's limits. For
// each request it resolves the caller identity and checks the limits. Allowed
// requests are forwarded. When a limit is violated and action is ActionWarn the
// violation is logged and the request is forwarded; otherwise the request is
// rejected with HTTP 403 and a provider-shaped JSON error body (the inner
// handler is not invoked).
func Middleware(e *Engine, resolve ResolveFunc, reg *provider.Registry, action Action, blockStatus int) func(http.Handler) http.Handler {
	if blockStatus == 0 {
		blockStatus = http.StatusPaymentRequired
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			devID, team := resolve(r)
			d := e.Check(devID, team)
			if d.Allowed {
				next.ServeHTTP(w, r)
				return
			}

			msg := violationMessage(d)

			if action == ActionWarn {
				log.Printf("[LIMIT] warn %s", msg)
				next.ServeHTTP(w, r)
				return
			}

			writeBlock(w, r, reg, msg, blockStatus)
		})
	}
}

// violationMessage renders the human-readable limit-exceeded message.
func violationMessage(d Decision) string {
	if d.Limit == nil {
		return "inferenced: spend limit exceeded"
	}
	l := d.Limit
	return fmt.Sprintf("inferenced: %s %q exceeded $%g/%s (spent $%g)",
		l.Scope, l.ID, l.LimitUSD, l.Period, d.SpentUSD)
}

// writeBlock renders the rejection at the given status using the detected
// provider's native error envelope, falling back to a generic JSON error when
// no provider is detected. The default status (402 Payment Required) is chosen
// so coding agents surface it as a quota/billing error rather than mapping it to
// an auth failure (401/403 make Claude Code prompt for re-login) or a retry
// (429).
func writeBlock(w http.ResponseWriter, r *http.Request, reg *provider.Registry, msg string, status int) {
	if reg != nil {
		if p, ok := reg.Detect(r); ok {
			ct, body := p.ErrorResponse(status, msg)
			w.Header().Set("Content-Type", ct)
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
	}

	body, err := json.Marshal(map[string]string{"error": msg})
	if err != nil {
		// json.Marshal of a map[string]string cannot fail; guard defensively.
		body = []byte(`{"error":"inferenced: spend limit exceeded"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
