package enforce_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Fraser-Isbester/inferenced/internal/enforce"
	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
)

func TestEnforce(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Enforce Suite")
}

// fixedClock returns a clock function pinned to t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

var _ = Describe("Engine.Check", func() {
	// A deterministic instant used for all period math assertions.
	now := time.Date(2026, time.July, 2, 15, 30, 0, 0, time.UTC)

	Describe("threshold behavior", func() {
		limits := []enforce.Limit{{
			Scope:    enforce.ScopeDeveloper,
			ID:       "dev-1",
			Period:   enforce.PeriodDay,
			LimitUSD: 10.0,
		}}

		It("allows when spend is under the ceiling", func() {
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 9.99, nil
			}, fixedClock(now))
			d := eng.Check("dev-1", "team-a")
			Expect(d.Allowed).To(BeTrue())
			Expect(d.Limit).To(BeNil())
		})

		It("blocks at exactly the ceiling", func() {
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 10.0, nil
			}, fixedClock(now))
			d := eng.Check("dev-1", "team-a")
			Expect(d.Allowed).To(BeFalse())
			Expect(d.Limit).NotTo(BeNil())
			Expect(d.SpentUSD).To(Equal(10.0))
		})

		It("blocks when spend is over the ceiling", func() {
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 42.0, nil
			}, fixedClock(now))
			d := eng.Check("dev-1", "team-a")
			Expect(d.Allowed).To(BeFalse())
			Expect(d.SpentUSD).To(Equal(42.0))
			Expect(d.Limit.Scope).To(Equal(enforce.ScopeDeveloper))
		})
	})

	Describe("scope matching", func() {
		It("applies a developer-scoped limit only to the matching developer", func() {
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeDeveloper,
				ID:       "dev-1",
				Period:   enforce.PeriodDay,
				LimitUSD: 1.0,
			}}
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 100.0, nil
			}, fixedClock(now))

			// Matching developer -> limit applies -> blocked.
			Expect(eng.Check("dev-1", "team-a").Allowed).To(BeFalse())
			// Different developer -> limit does not apply -> allowed.
			Expect(eng.Check("dev-2", "team-a").Allowed).To(BeTrue())
		})

		It("applies a team-scoped limit only to the matching team", func() {
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeTeam,
				ID:       "team-a",
				Period:   enforce.PeriodDay,
				LimitUSD: 1.0,
			}}
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 100.0, nil
			}, fixedClock(now))

			Expect(eng.Check("dev-1", "team-a").Allowed).To(BeFalse())
			Expect(eng.Check("dev-1", "team-b").Allowed).To(BeTrue())
		})

		It("passes the correct scope column to the spend function", func() {
			var gotScope, gotID string
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeTeam,
				ID:       "team-a",
				Period:   enforce.PeriodDay,
				LimitUSD: 100.0,
			}}
			eng := enforce.NewEngine(limits, func(scope, id string, _ time.Time) (float64, error) {
				gotScope, gotID = scope, id
				return 0, nil
			}, fixedClock(now))

			eng.Check("dev-1", "team-a")
			Expect(gotScope).To(Equal("team"))
			Expect(gotID).To(Equal("team-a"))
		})
	})

	Describe("wildcard id", func() {
		It("applies a developer wildcard to any developer and forwards \"*\" as the id", func() {
			var gotID string
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeDeveloper,
				ID:       "*",
				Period:   enforce.PeriodDay,
				LimitUSD: 5.0,
			}}
			eng := enforce.NewEngine(limits, func(_, id string, _ time.Time) (float64, error) {
				gotID = id
				return 5.0, nil
			}, fixedClock(now))

			d := eng.Check("anyone", "team-z")
			Expect(d.Allowed).To(BeFalse())
			Expect(gotID).To(Equal("*"))
		})

		It("applies a team wildcard to any team", func() {
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeTeam,
				ID:       "*",
				Period:   enforce.PeriodMonth,
				LimitUSD: 5.0,
			}}
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 6.0, nil
			}, fixedClock(now))
			Expect(eng.Check("dev-1", "whatever").Allowed).To(BeFalse())
		})
	})

	Describe("period start computation", func() {
		var gotSince time.Time
		spend := func(_, _ string, since time.Time) (float64, error) {
			gotSince = since
			return 0, nil
		}

		It("uses local midnight for a day period", func() {
			limits := []enforce.Limit{{
				Scope: enforce.ScopeDeveloper, ID: "*", Period: enforce.PeriodDay, LimitUSD: 100,
			}}
			enforce.NewEngine(limits, spend, fixedClock(now)).Check("d", "t")
			Expect(gotSince).To(Equal(time.Date(2026, time.July, 2, 0, 0, 0, 0, time.UTC)))
		})

		It("uses the first of the month for a month period", func() {
			limits := []enforce.Limit{{
				Scope: enforce.ScopeDeveloper, ID: "*", Period: enforce.PeriodMonth, LimitUSD: 100,
			}}
			enforce.NewEngine(limits, spend, fixedClock(now)).Check("d", "t")
			Expect(gotSince).To(Equal(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)))
		})

		It("uses now minus window for a rolling period", func() {
			limits := []enforce.Limit{{
				Scope:    enforce.ScopeDeveloper,
				ID:       "*",
				Period:   enforce.PeriodRolling,
				Window:   24 * time.Hour,
				LimitUSD: 100,
			}}
			enforce.NewEngine(limits, spend, fixedClock(now)).Check("d", "t")
			Expect(gotSince).To(Equal(now.Add(-24 * time.Hour)))
		})
	})

	Describe("spend errors fail open", func() {
		It("allows the request when the spend function errors", func() {
			limits := []enforce.Limit{{
				Scope: enforce.ScopeDeveloper, ID: "*", Period: enforce.PeriodDay, LimitUSD: 0.0,
			}}
			eng := enforce.NewEngine(limits, func(string, string, time.Time) (float64, error) {
				return 0, errBoom
			}, fixedClock(now))
			// Even though LimitUSD is 0 (which 0 spend would violate), the query
			// error causes a fail-open allow.
			Expect(eng.Check("d", "t").Allowed).To(BeTrue())
		})
	})
})

// errBoom is a sentinel spend-query error used by the fail-open spec.
var errBoom = &boomError{}

type boomError struct{}

func (*boomError) Error() string { return "boom" }

var _ = Describe("Middleware", func() {
	now := time.Date(2026, time.July, 2, 15, 30, 0, 0, time.UTC)

	// resolve is a fixed identity resolver for the middleware specs.
	resolve := func(*http.Request) (string, string) { return "dev-1", "team-a" }

	newReg := func() *provider.Registry {
		reg := provider.NewRegistry()
		reg.Register(anthropic.New())
		return reg
	}

	overLimit := []enforce.Limit{{
		Scope: enforce.ScopeDeveloper, ID: "*", Period: enforce.PeriodDay, LimitUSD: 1.0,
	}}

	It("blocks with a provider-shaped 403 and does not call the inner handler", func() {
		called := false
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		eng := enforce.NewEngine(overLimit, func(string, string, time.Time) (float64, error) {
			return 99.0, nil
		}, fixedClock(now))

		h := enforce.Middleware(eng, resolve, newReg(), enforce.ActionBlock, http.StatusPaymentRequired)(inner)
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		Expect(called).To(BeFalse())
		Expect(rec.Code).To(Equal(http.StatusPaymentRequired))
		Expect(rec.Header().Get("Content-Type")).To(Equal("application/json"))

		var body map[string]any
		Expect(json.Unmarshal(rec.Body.Bytes(), &body)).To(Succeed())
		Expect(body["type"]).To(Equal("error"))
		errObj, ok := body["error"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(errObj["type"]).To(Equal("invalid_request_error"))
	})

	It("forwards to the inner handler in warn mode even when over limit", func() {
		called := false
		innerH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusTeapot)
		})
		eng := enforce.NewEngine(overLimit, func(string, string, time.Time) (float64, error) {
			return 99.0, nil
		}, fixedClock(now))

		h := enforce.Middleware(eng, resolve, newReg(), enforce.ActionWarn, http.StatusPaymentRequired)(innerH)
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		Expect(called).To(BeTrue())
		Expect(rec.Code).To(Equal(http.StatusTeapot))
	})

	It("forwards allowed requests", func() {
		called := false
		innerH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		})
		eng := enforce.NewEngine(overLimit, func(string, string, time.Time) (float64, error) {
			return 0.0, nil
		}, fixedClock(now))

		h := enforce.Middleware(eng, resolve, newReg(), enforce.ActionBlock, http.StatusPaymentRequired)(innerH)
		req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		Expect(called).To(BeTrue())
		Expect(rec.Code).To(Equal(http.StatusOK))
	})

	It("falls back to a generic JSON error when no provider is detected", func() {
		inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		eng := enforce.NewEngine(overLimit, func(string, string, time.Time) (float64, error) {
			return 99.0, nil
		}, fixedClock(now))

		h := enforce.Middleware(eng, resolve, newReg(), enforce.ActionBlock, http.StatusPaymentRequired)(inner)
		// A host/path that no registered provider recognizes.
		req := httptest.NewRequest(http.MethodGet, "http://example.com/nope", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		Expect(rec.Code).To(Equal(http.StatusPaymentRequired))
		Expect(rec.Header().Get("Content-Type")).To(Equal("application/json"))
		var body map[string]string
		Expect(json.Unmarshal(rec.Body.Bytes(), &body)).To(Succeed())
		Expect(body["error"]).To(ContainSubstring("inferenced:"))
	})
})
