// Package uiserver serves the local dashboard for inferenced: spend summaries, a
// live feed of metered requests, spend-vs-limit gauges, and a rules editor. It
// reads the SQLite ledger directly (the shared source of truth) rather than
// sharing in-memory state with the running gateway, so "realtime" is a ~1s tail
// of new ledger rows over Server-Sent Events. Config is read fresh from disk per
// request (so gauges/editor reflect what the gateway enforces); editing writes
// the config file, which a running `serve` hot-reloads.
package uiserver

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/store"
)

// loadCfg reads the config at cfgPath, returning defaults when the file does not
// exist yet. The dashboard must work before any config has been written, and
// resolveConfigPath yields a concrete (possibly non-existent) default path that
// config.Load would otherwise treat as a hard error.
func loadCfg(cfgPath string) (*config.Config, error) {
	if cfgPath != "" {
		if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
			return config.Default(), nil
		}
	}
	return config.Load(cfgPath)
}

//go:embed assets/index.html
var indexHTML []byte

const (
	// recentLimit is how many past requests seed the live feed on connect.
	recentLimit = 50
	// tailLimit caps rows drained per poll so a burst cannot flood one write.
	tailLimit = 500
	// tailInterval is how often the SSE handler polls the ledger for new rows.
	tailInterval = time.Second
)

// Store is the read surface the dashboard needs from the ledger.
type Store interface {
	SpendReport(since time.Time) (store.Report, error)
	RecentEvents(limit int) ([]store.EventRow, int64, error)
	EventsSince(sinceRowID int64, limit int) ([]store.EventRow, int64, error)
	EventMetadata(rowID int64) (string, error)
	Spend(scope, id string, since time.Time) (float64, error)
	ListArtifacts(since time.Time, limit int) ([]store.ArtifactRow, error)
	SubjectStats(since time.Time) ([]store.SubjectStat, error)
	EventByRequest(requestID string, tsUnixMs int64) (store.EventDetail, error)
}

// Handler builds the dashboard HTTP handler. st is the ledger read handle;
// cfgPath is the resolved config file path, loaded fresh per request so gauges
// and the editor reflect what the gateway enforces, and written on edit.
func Handler(st Store, cfgPath string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Never cache the shell, so a rebuilt gateway always serves fresh assets.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})

	mux.HandleFunc("/api/spend", func(w http.ResponseWriter, r *http.Request) {
		since := periodStart(r.URL.Query().Get("period"), r.URL.Query().Get("window"), time.Now())
		report, err := st.SpendReport(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, report)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, _ *http.Request) {
		cfg, err := loadCfg(cfgPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b, err := yaml.Marshal(cfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(b)
	})

	mux.HandleFunc("/api/limits", func(w http.ResponseWriter, _ *http.Request) {
		gauges, err := limitGauges(st, cfgPath, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, gauges)
	})

	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, _ *http.Request) {
		ov, err := buildOverview(st, cfgPath, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, ov)
	})

	mux.HandleFunc("/api/rules", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, err := loadCfg(cfgPath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, rulesFrom(cfg))
		case http.MethodPut:
			handleRulesPut(w, r, cfgPath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/limit", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			handleLimitPut(w, r, cfgPath)
		case http.MethodDelete:
			handleLimitDelete(w, r, cfgPath)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/event", func(w http.ResponseWriter, r *http.Request) {
		rowID, err := strconv.ParseInt(r.URL.Query().Get("row_id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid row_id", http.StatusBadRequest)
			return
		}
		meta, err := st.EventMetadata(rowID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		// meta is a JSON string (or empty); wrap it as raw JSON so the client
		// receives the blob directly under "metadata".
		if meta == "" {
			meta = "null"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":`))
		_, _ = w.Write([]byte(meta))
		_, _ = w.Write([]byte("}"))
	})

	mux.HandleFunc("/api/artifacts", func(w http.ResponseWriter, r *http.Request) {
		since := periodStart(r.URL.Query().Get("period"), r.URL.Query().Get("window"), time.Now())
		rows, err := st.ListArtifacts(since, 500)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, rows)
	})

	mux.HandleFunc("/api/request", func(w http.ResponseWriter, r *http.Request) {
		ts, err := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
		if err != nil {
			http.Error(w, "invalid ts", http.StatusBadRequest)
			return
		}
		d, err := st.EventByRequest(r.URL.Query().Get("id"), ts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, d)
	})

	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		streamEvents(w, r, st)
	})

	return mux
}

// streamEvents serves the live request feed as Server-Sent Events: it replays
// the most recent rows, then tails new ledger rows every tailInterval until the
// client disconnects.
func streamEvents(w http.ResponseWriter, r *http.Request, st Store) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	recent, lastRowID, err := st.RecentEvents(recentLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, e := range recent {
		writeSSE(w, e)
	}
	flusher.Flush()

	ticker := time.NewTicker(tailInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			rows, maxRowID, err := st.EventsSince(lastRowID, tailLimit)
			if err != nil {
				return
			}
			for _, e := range rows {
				writeSSE(w, e)
			}
			if len(rows) > 0 {
				flusher.Flush()
			}
			lastRowID = maxRowID
		}
	}
}

// writeSSE writes one event row as an SSE "data:" frame. Marshal errors are
// skipped rather than breaking the stream.
func writeSSE(w http.ResponseWriter, e store.EventRow) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(b)
	_, _ = w.Write([]byte("\n\n"))
}

// writeJSON encodes v as a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// periodStart converts a period selector into the inclusive lower bound for a
// spend query, mirroring the enforcement period semantics. Unknown periods
// default to the start of the current local day.
func periodStart(period, window string, now time.Time) time.Time {
	switch period {
	case "month":
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	case "rolling":
		d := 7 * 24 * time.Hour
		if window != "" {
			if parsed, err := time.ParseDuration(window); err == nil {
				d = parsed
			}
		}
		return now.Add(-d)
	default: // "day" or empty
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
}
