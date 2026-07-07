package uiserver

import (
	"log"
	"sort"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/enforce"
)

// discoveryWindow is how far back the overview looks to list the subjects
// (developers/teams) active on this machine, independent of any limit period.
const discoveryWindow = 30 * 24 * time.Hour

// subject is one row of the overview: a developer or team with its current
// standing against the applicable limit.
type subject struct {
	Kind                  string  `json:"kind"` // "user" | "team"
	ID                    string  `json:"id"`
	Client                string  `json:"client,omitempty"`
	SpentUSD              float64 `json:"spent_usd"`
	LimitUSD              float64 `json:"limit_usd"`
	Period                string  `json:"period,omitempty"`
	DowngradeThresholdUSD float64 `json:"downgrade_threshold_usd,omitempty"`
	BlockThresholdUSD     float64 `json:"block_threshold_usd,omitempty"`
	Status                string  `json:"status"` // ok | alerting | downgrading | enforced | none
	Pct                   int     `json:"pct"`
}

// overviewResponse is the payload for GET /api/overview.
type overviewResponse struct {
	Enforcement struct {
		Enabled bool   `json:"enabled"`
		Action  string `json:"action"`
	} `json:"enforcement"`
	Totals struct {
		TodayUSD       float64 `json:"today_usd"`
		TodayRequests  int     `json:"today_requests"`
		WindowUSD      float64 `json:"window_usd"`
		WindowRequests int     `json:"window_requests"`
		WindowDays     int     `json:"window_days"`
	} `json:"totals"`
	Downgrades []string  `json:"downgrades"` // e.g. ["opus → claude-sonnet-4-5"]
	Subjects   []subject `json:"subjects"`
}

// buildOverview assembles the machine overview: every active developer/team plus
// any explicitly-configured limit target, each scored against its applicable
// limit over that limit's own period.
func buildOverview(st Store, cfgPath string, now time.Time) (overviewResponse, error) {
	var resp overviewResponse
	cfg, err := loadCfg(cfgPath)
	if err != nil {
		return resp, err
	}
	limits, err := cfg.EnforceLimits()
	if err != nil {
		return resp, err
	}
	resp.Enforcement.Enabled = cfg.Enforcement.Enabled
	resp.Enforcement.Action = string(cfg.EnforceAction())
	for _, d := range cfg.DowngradeRules() {
		resp.Downgrades = append(resp.Downgrades, d.Match+" → "+d.To)
	}

	// Headline totals: today and the trailing discovery window.
	if rep, err := st.SpendReport(periodStart("day", "", now)); err == nil {
		resp.Totals.TodayUSD = rep.TotalUSD
		resp.Totals.TodayRequests = rep.TotalRequests
	}
	if rep, err := st.SpendReport(now.Add(-discoveryWindow)); err == nil {
		resp.Totals.WindowUSD = rep.TotalUSD
		resp.Totals.WindowRequests = rep.TotalRequests
	}
	resp.Totals.WindowDays = int(discoveryWindow / (24 * time.Hour))

	// A stats query hiccup degrades to config-only subjects rather than blanking
	// the whole panel (and its totals); config errors above are still fatal.
	stats, err := st.SubjectStats(now.Add(-discoveryWindow))
	if err != nil {
		log.Printf("overview: subject stats: %v", err)
		stats = nil
	}

	// Seed the subject set from active subjects, plus any concrete (non-wildcard)
	// configured limit target so a configured-but-idle team still appears.
	type key struct{ kind, id string }
	seen := map[key]bool{}
	clients := map[string]string{}
	spentByKey := map[key]float64{} // spend already bucketed by SubjectStats
	order := []key{}
	addKey := func(kind, id string) {
		k := key{kind, id}
		if id == "" || id == "*" || seen[k] {
			return
		}
		seen[k] = true
		order = append(order, k)
	}
	for _, s := range stats {
		addKey(s.Kind, s.ID)
		spentByKey[key{s.Kind, s.ID}] = s.SpentUSD
		if s.Kind == "user" {
			clients[s.ID] = s.Client
		}
	}
	for _, l := range limits {
		if l.Scope == enforce.ScopeDeveloper {
			addKey("user", l.ID)
		} else {
			addKey("team", l.ID)
		}
	}

	for _, k := range order {
		scope := enforce.ScopeTeam
		if k.kind == "user" {
			scope = enforce.ScopeDeveloper
		}
		s := subject{Kind: k.kind, ID: k.id, Client: clients[k.id]}

		if l, ok := applicableLimit(limits, scope, k.id); ok {
			window := ""
			if l.Window > 0 {
				window = l.Window.String()
			}
			since := periodStart(string(l.Period), window, now)
			spent, _ := st.Spend(string(scope), k.id, since)
			blockAt := l.BlockAt
			if blockAt <= 0 {
				blockAt = 1.0
			}
			s.SpentUSD = spent
			s.LimitUSD = l.LimitUSD
			s.Period = string(l.Period)
			s.BlockThresholdUSD = blockAt * l.LimitUSD
			if l.DowngradeAt > 0 {
				s.DowngradeThresholdUSD = l.DowngradeAt * l.LimitUSD
			}
			s.Status = statusFor(spent, s.DowngradeThresholdUSD, s.BlockThresholdUSD)
			if l.LimitUSD > 0 {
				s.Pct = int(spent / l.LimitUSD * 100)
			}
		} else {
			// No limit: use the spend already bucketed by SubjectStats (which
			// folds team-less usage into "unknown"); a re-query by exact id would
			// miss those rows.
			s.SpentUSD = spentByKey[k]
			s.Status = "none"
		}
		resp.Subjects = append(resp.Subjects, s)
	}

	// Users first, then teams; within each group, most at-risk first.
	sort.SliceStable(resp.Subjects, func(i, j int) bool {
		a, b := resp.Subjects[i], resp.Subjects[j]
		if (a.Kind == "user") != (b.Kind == "user") {
			return a.Kind == "user"
		}
		if a.Pct != b.Pct {
			return a.Pct > b.Pct
		}
		return a.SpentUSD > b.SpentUSD
	})
	return resp, nil
}

// statusFor derives a subject's status from its spend and thresholds. Block
// wins; then downgrade; then an 80%-of-block "alerting" warning; else ok.
func statusFor(spent, downgradeThreshold, blockThreshold float64) string {
	switch {
	case blockThreshold > 0 && spent >= blockThreshold:
		return "enforced"
	case downgradeThreshold > 0 && spent >= downgradeThreshold:
		return "downgrading"
	case blockThreshold > 0 && spent >= 0.8*blockThreshold:
		return "alerting"
	default:
		return "ok"
	}
}

// applicableLimit returns the limit governing a scope/id, preferring an exact id
// match over a "*" wildcard.
func applicableLimit(limits []enforce.Limit, scope enforce.Scope, id string) (enforce.Limit, bool) {
	var wildcard *enforce.Limit
	for i := range limits {
		l := limits[i]
		if l.Scope != scope {
			continue
		}
		if l.ID == id {
			return l, true
		}
		if l.ID == "*" && wildcard == nil {
			wildcard = &limits[i]
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return enforce.Limit{}, false
}
