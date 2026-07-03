package uiserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/config"
)

// limitGauge is one spend-vs-limit gauge for the dashboard: the configured limit
// plus the current spend and the absolute USD thresholds derived from the
// downgrade/block fractions.
type limitGauge struct {
	Scope                 string  `json:"scope"`
	ID                    string  `json:"id"`
	Period                string  `json:"period"`
	Window                string  `json:"window,omitempty"`
	LimitUSD              float64 `json:"limit_usd"`
	DowngradeAt           float64 `json:"downgrade_at,omitempty"`
	BlockAt               float64 `json:"block_at,omitempty"`
	SpentUSD              float64 `json:"spent_usd"`
	DowngradeThresholdUSD float64 `json:"downgrade_threshold_usd,omitempty"`
	BlockThresholdUSD     float64 `json:"block_threshold_usd"`
}

// limitGauges loads the configured (and validated) limits and computes the
// current spend and thresholds for each, over each limit's own period. A spend
// lookup error fails soft to 0 so one bad row never blanks the whole view.
func limitGauges(st Store, cfgPath string, now time.Time) ([]limitGauge, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	limits, err := cfg.EnforceLimits()
	if err != nil {
		return nil, err
	}

	out := make([]limitGauge, 0, len(limits))
	for _, l := range limits {
		window := ""
		if l.Window > 0 {
			window = l.Window.String()
		}
		spent, err := st.Spend(string(l.Scope), l.ID, periodStart(string(l.Period), window, now))
		if err != nil {
			spent = 0
		}
		blockAt := l.BlockAt
		if blockAt <= 0 {
			blockAt = 1.0
		}
		g := limitGauge{
			Scope: string(l.Scope), ID: l.ID, Period: string(l.Period), Window: window,
			LimitUSD: l.LimitUSD, DowngradeAt: l.DowngradeAt, BlockAt: l.BlockAt,
			SpentUSD:          spent,
			BlockThresholdUSD: blockAt * l.LimitUSD,
		}
		if l.DowngradeAt > 0 {
			g.DowngradeThresholdUSD = l.DowngradeAt * l.LimitUSD
		}
		out = append(out, g)
	}
	return out, nil
}

// rulesDTO is the JSON shape of the enforcement block exchanged with the editor.
// It mirrors config.Enforcement with stable snake_case keys, decoupled from the
// config package's YAML tags.
type rulesDTO struct {
	Enabled     bool           `json:"enabled"`
	Action      string         `json:"action"`
	BlockStatus int            `json:"block_status,omitempty"`
	Downgrade   []downgradeDTO `json:"downgrade"`
	Limits      []limitDTO     `json:"limits"`
}

type downgradeDTO struct {
	Match string `json:"match"`
	To    string `json:"to"`
}

type limitDTO struct {
	Scope       string  `json:"scope"`
	ID          string  `json:"id"`
	Period      string  `json:"period"`
	Window      string  `json:"window,omitempty"`
	LimitUSD    float64 `json:"limit_usd"`
	DowngradeAt float64 `json:"downgrade_at,omitempty"`
	BlockAt     float64 `json:"block_at,omitempty"`
}

// rulesFrom projects a config's enforcement block into the wire DTO.
func rulesFrom(cfg *config.Config) rulesDTO {
	e := cfg.Enforcement
	dto := rulesDTO{Enabled: e.Enabled, Action: e.Action, BlockStatus: e.BlockStatus}
	for _, d := range e.Downgrade {
		dto.Downgrade = append(dto.Downgrade, downgradeDTO{Match: d.Match, To: d.To})
	}
	for _, l := range e.Limits {
		dto.Limits = append(dto.Limits, limitDTO{
			Scope: l.Scope, ID: l.ID, Period: l.Period, Window: l.Window,
			LimitUSD: l.LimitUSD, DowngradeAt: l.DowngradeAt, BlockAt: l.BlockAt,
		})
	}
	return dto
}

// toEnforcement converts the wire DTO back into a config.Enforcement block.
func (dto rulesDTO) toEnforcement() config.Enforcement {
	e := config.Enforcement{Enabled: dto.Enabled, Action: dto.Action, BlockStatus: dto.BlockStatus}
	for _, d := range dto.Downgrade {
		e.Downgrade = append(e.Downgrade, config.DowngradeRuleYAML{Match: d.Match, To: d.To})
	}
	for _, l := range dto.Limits {
		e.Limits = append(e.Limits, config.LimitYAML{
			Scope: l.Scope, ID: l.ID, Period: l.Period, Window: l.Window,
			LimitUSD: l.LimitUSD, DowngradeAt: l.DowngradeAt, BlockAt: l.BlockAt,
		})
	}
	return e
}

// handleRulesPut validates an edited enforcement block and, if valid, writes it
// back to the config file (which a running gateway hot-reloads). Invalid input
// returns 400 and leaves the file untouched.
func handleRulesPut(w http.ResponseWriter, r *http.Request, cfgPath string) {
	var dto rulesDTO
	if err := json.NewDecoder(r.Body).Decode(&dto); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cfg.Enforcement = dto.toEnforcement()
	// Validate before persisting so a bad edit can't be saved (and then rejected
	// on the gateway's reload).
	if _, err := cfg.EnforceLimits(); err != nil {
		http.Error(w, "invalid rules: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := cfg.Save(cfgPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
