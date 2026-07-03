// Package enforce evaluates local spend limits against a pluggable spend
// source and, via HTTP middleware, blocks or warns on over-limit inference
// requests with a provider-shaped error.
//
// The engine is decoupled from persistence and configuration: callers inject a
// SpendFunc (typically backed by the local SQLite ledger) and a clock, so
// period math and limit evaluation are fully testable without a database.
package enforce

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
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

// Limit describes a single spend ceiling for a scope/id over a period, with
// optional per-limit downgrade and block thresholds expressed as fractions of
// LimitUSD. This makes enforcement rule-based and dynamic: one team can downgrade
// at 80% and block at 120% while another simply blocks at 100%.
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
	// LimitUSD is the reference budget in US dollars; the thresholds below are
	// fractions of it.
	LimitUSD float64
	// DowngradeAt is the fraction of LimitUSD at or above which requests are
	// model-downgraded (in enforce mode). Zero disables downgrade for this limit.
	DowngradeAt float64
	// BlockAt is the fraction of LimitUSD at or above which requests are blocked.
	// Zero means the default of 1.0 (block once spend reaches the budget).
	BlockAt float64
}

// blockThreshold returns the absolute USD spend at which this limit blocks.
func (l Limit) blockThreshold() float64 {
	at := l.BlockAt
	if at <= 0 {
		at = 1.0
	}
	return at * l.LimitUSD
}

// downgradeThreshold returns the absolute USD spend at which this limit
// downgrades, and whether downgrade is enabled for the limit.
func (l Limit) downgradeThreshold() (float64, bool) {
	if l.DowngradeAt <= 0 {
		return 0, false
	}
	return l.DowngradeAt * l.LimitUSD, true
}

// Rule maps a model matcher to its downgrade target. When a request is
// downgraded, the first rule whose Match is a substring of the request model
// (case-insensitive) determines the replacement model.
type Rule struct {
	// Match is a lowercase substring tested against the request model.
	Match string
	// To is the replacement model id.
	To string
}

// SpendFunc returns the total spend in US dollars for the given scope and id
// since the given time. An id of "*" means "all ids within the scope".
type SpendFunc func(scope, id string, since time.Time) (float64, error)

// Decision is the result of evaluating the configured limits for a caller.
type Decision struct {
	// Allowed reports whether the request should proceed (possibly downgraded).
	// It is false only when a limit's block threshold is reached.
	Allowed bool
	// Downgrade reports whether the request should proceed but with its model
	// down-scoped. It is never true when Allowed is false (block wins).
	Downgrade bool
	// Limit is the limit that determined the outcome: the blocking limit when
	// Allowed is false, or the first downgrade-triggering limit when Downgrade is
	// true. Nil when the request is allowed unchanged.
	Limit *Limit
	// SpentUSD is the spend observed for that limit.
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

// Check evaluates every applicable limit for the given developer and team. A
// limit applies when its scope matches and its id is "*" or equals the caller's
// id for that scope. Block always wins: if any applicable limit reaches its
// block threshold the request is blocked; otherwise, if any limit reaches its
// downgrade threshold, the request is allowed but flagged for model down-scoping
// (the first such limit is reported). Spend-lookup errors fail open (that limit
// is skipped) and are logged.
func (e *Engine) Check(devID, team string) Decision {
	now := e.clock()
	var (
		dgLimit *Limit
		dgSpent float64
	)
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
		if l.LimitUSD <= 0 {
			continue
		}

		start := periodStart(l, now)
		spent, err := e.spend(string(l.Scope), l.ID, start)
		if err != nil {
			// Fail open: never block traffic because the ledger query failed.
			log.Printf("[LIMIT] spend query error: scope=%s id=%q: %v", l.Scope, l.ID, err)
			continue
		}

		if spent >= l.blockThreshold() {
			blocked := l
			return Decision{Allowed: false, Limit: &blocked, SpentUSD: spent}
		}
		if dgLimit == nil {
			if at, ok := l.downgradeThreshold(); ok && spent >= at {
				triggered := l
				dgLimit = &triggered
				dgSpent = spent
			}
		}
	}
	if dgLimit != nil {
		return Decision{Allowed: true, Downgrade: true, Limit: dgLimit, SpentUSD: dgSpent}
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
// each request it resolves the caller identity and checks the limits.
//
//   - ActionWarn is a dry run: any block or downgrade intent is logged and the
//     request is forwarded unchanged.
//   - Otherwise (enforce mode): a blocked request is rejected with a
//     provider-shaped error at blockStatus; a downgrade-flagged request has its
//     model rewritten per rules and is forwarded; an allowed request is
//     forwarded unchanged.
//
// rules is the ordered downgrade map (first substring match wins).
func Middleware(e *Engine, resolve ResolveFunc, reg *provider.Registry, action Action, blockStatus int, rules []Rule) func(http.Handler) http.Handler {
	rs := Ruleset{Engine: e, Action: action, BlockStatus: blockStatus, Rules: rules}
	return DynamicMiddleware(resolve, reg, func() Ruleset { return rs })
}

// Ruleset is the enforcement configuration evaluated for a single request. It
// bundles the engine (limits), the over-limit action, the block status, and the
// downgrade rules so the whole set can be swapped atomically on config reload.
type Ruleset struct {
	Engine      *Engine
	Action      Action
	BlockStatus int
	Rules       []Rule
}

// DynamicMiddleware is like Middleware but reads the current Ruleset per request
// via get, so a running gateway can hot-reload enforcement (swap the Ruleset)
// without rebuilding the handler chain. get must be safe for concurrent use and
// must return a Ruleset with a non-nil Engine.
func DynamicMiddleware(resolve ResolveFunc, reg *provider.Registry, get func() Ruleset) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rs := get()
			blockStatus := rs.BlockStatus
			if blockStatus == 0 {
				blockStatus = http.StatusPaymentRequired
			}
			d := rs.Engine.Check(resolve(r))

			if rs.Action == ActionWarn {
				switch {
				case !d.Allowed:
					log.Printf("[LIMIT] warn %s", violationMessage(d))
				case d.Downgrade:
					log.Printf("[LIMIT] warn would downgrade: %s", violationMessage(d))
				}
				next.ServeHTTP(w, r)
				return
			}

			if !d.Allowed {
				writeBlock(w, r, reg, violationMessage(d), blockStatus)
				return
			}

			if d.Downgrade {
				applyDowngrade(r, reg, rs.Rules, d)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// applyDowngrade rewrites the request's model to the configured downgrade target
// before it is forwarded. It is best-effort: any failure (no provider, no
// matching rule, read/rewrite error) forwards the request unchanged rather than
// dropping it, since a downgrade must never break a request the budget still
// permits.
func applyDowngrade(r *http.Request, reg *provider.Registry, rules []Rule, d Decision) {
	if reg == nil || len(rules) == 0 || r.Body == nil {
		return
	}
	p, ok := reg.Detect(r)
	if !ok {
		return
	}

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		log.Printf("[DOWNGRADE] body read error, forwarding unchanged: %v", err)
		return
	}

	model := p.ParseRequest(r, body).Model
	target, matched := matchRule(rules, model)
	if !matched || target == "" || strings.EqualFold(target, model) {
		// No applicable rule or already at the target: forward unchanged.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return
	}

	newBody, err := p.RewriteModel(r, body, target)
	if err != nil {
		log.Printf("[DOWNGRADE] rewrite %q -> %q failed, forwarding unchanged: %v", model, target, err)
		newBody = body
	} else {
		log.Printf("[DOWNGRADE] %q -> %q (%s)", model, target, violationMessage(d))
	}
	r.Body = io.NopCloser(bytes.NewReader(newBody))
	r.ContentLength = int64(len(newBody))
	// Drop any stale Content-Length header; the proxy re-derives it from Body.
	r.Header.Del("Content-Length")
}

// matchRule returns the downgrade target for a model: the To of the first rule
// whose Match is a substring of the model (case-insensitive).
func matchRule(rules []Rule, model string) (string, bool) {
	m := strings.ToLower(model)
	for _, rule := range rules {
		if rule.Match == "" {
			continue
		}
		if strings.Contains(m, strings.ToLower(rule.Match)) {
			return rule.To, true
		}
	}
	return "", false
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
