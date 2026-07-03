package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// DefaultRemoteURL is the community-maintained models.dev pricing aggregate.
// There is no first-party per-provider pricing API; this JSON is the pragmatic
// live source and is refreshed on boot behind a TTL cache.
const DefaultRemoteURL = "https://models.dev/api.json"

// DefaultTTL is how long a cached remote sheet is considered fresh.
const DefaultTTL = 24 * time.Hour

// providerAlias maps a models.dev provider id to the inferenced provider name.
// Only providers with a built-in inferenced provider are mapped; the rest of
// the models.dev catalog is ignored. Google's catalog is inferenced's "gemini".
var providerAlias = map[string]string{
	"anthropic": "anthropic",
	"openai":    "openai",
	"google":    "gemini",
}

// modelsDevDoc is the subset of the models.dev api.json shape we consume.
type modelsDevDoc map[string]struct {
	Models map[string]struct {
		Cost struct {
			Input      float64 `json:"input"`
			Output     float64 `json:"output"`
			CacheRead  float64 `json:"cache_read"`
			CacheWrite float64 `json:"cache_write"`
		} `json:"cost"`
	} `json:"models"`
}

// parseModelsDev converts a models.dev api.json body into a Sheet keyed by
// inferenced provider names. Models with no input/output cost are skipped so
// they do not shadow the embedded family defaults with a zero price.
func parseModelsDev(body []byte) (*Sheet, error) {
	var doc modelsDevDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse models.dev payload: %w", err)
	}

	out := make(map[string]map[string]Price)
	for provID, prov := range doc {
		name, ok := providerAlias[provID]
		if !ok {
			continue
		}
		models := out[name]
		if models == nil {
			models = make(map[string]Price)
			out[name] = models
		}
		for modelID, m := range prov.Models {
			if m.Cost.Input == 0 && m.Cost.Output == 0 {
				continue
			}
			models[modelID] = Price{
				InputPer1M:      m.Cost.Input,
				OutputPer1M:     m.Cost.Output,
				CacheReadPer1M:  m.Cost.CacheRead,
				CacheWritePer1M: m.Cost.CacheWrite,
			}
		}
	}
	return NewSheet(out), nil
}

// RemoteOptions configures the boot-time remote sheet load.
type RemoteOptions struct {
	// URL is the pricing source; empty uses DefaultRemoteURL.
	URL string
	// TTL is how long a cached sheet stays fresh; zero uses DefaultTTL.
	TTL time.Duration
	// CachePath is where the fetched payload is cached; empty uses the default
	// XDG cache location.
	CachePath string
	// Client is the HTTP client used to fetch; nil uses a client with a short
	// timeout so a slow source cannot stall gateway startup.
	Client *http.Client
}

// LoadRemote returns a live pricing Sheet, preferring a fresh on-disk cache and
// otherwise fetching from the remote source and refreshing the cache. It is
// designed to never fail startup: on any error (network, parse, cache I/O) it
// falls back to a stale cache if one exists, and returns a nil Sheet with an
// error only when nothing usable is available. Callers should treat a nil Sheet
// as "no remote prices" and continue with the embedded defaults.
func LoadRemote(ctx context.Context, opts RemoteOptions) (*Sheet, error) {
	url := opts.URL
	if url == "" {
		url = DefaultRemoteURL
	}
	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	cachePath := opts.CachePath
	if cachePath == "" {
		p, err := cachePathFallback()
		if err != nil {
			return nil, err
		}
		cachePath = p
	}

	// Serve a fresh cache without touching the network.
	if info, err := os.Stat(cachePath); err == nil {
		if age := time.Since(info.ModTime()); age < ttl {
			if sheet, err := loadCache(cachePath); err == nil {
				return sheet, nil
			}
		}
	}

	body, err := fetch(ctx, opts.Client, url)
	if err != nil {
		// Network failed: fall back to a stale cache if we have one.
		if sheet, cerr := loadCache(cachePath); cerr == nil {
			return sheet, nil
		}
		return nil, err
	}

	sheet, err := parseModelsDev(body)
	if err != nil {
		if sheet, cerr := loadCache(cachePath); cerr == nil {
			return sheet, nil
		}
		return nil, err
	}

	// Best-effort cache write; a write failure does not fail the load.
	writeCache(cachePath, body)
	return sheet, nil
}

// fetch retrieves the pricing payload over HTTP.
func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build pricing request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch pricing %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch pricing %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read pricing body: %w", err)
	}
	return body, nil
}

// loadCache reads and parses a cached models.dev payload.
func loadCache(path string) (*Sheet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseModelsDev(b)
}

// writeCache persists the raw payload to path, creating parent directories. It
// is best-effort: errors are ignored by the caller.
func writeCache(path string, body []byte) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	_ = os.WriteFile(path, body, 0o644)
}
