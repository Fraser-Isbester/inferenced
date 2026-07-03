package cli

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/enforce"
	"github.com/Fraser-Isbester/inferenced/internal/identity"
	"github.com/Fraser-Isbester/inferenced/internal/otelexport"
	"github.com/Fraser-Isbester/inferenced/internal/pricing"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// init registers the serve command.
func init() { register(newServeCmd) }

// newServeCmd builds the "serve" command, which runs the local reverse proxy.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the local inference gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			configPath, _ := cmd.Flags().GetString("config")
			port, _ := cmd.Flags().GetString("port")
			runServe(configPath, port)
			return nil
		},
	}
	cmd.Flags().String("port", "", "TCP port to listen on (overrides config listen address)")
	return cmd
}

// runServe holds the gateway body moved verbatim from the former command main.
// Behavior is identical to the previous binary: fatal errors call log.Fatalf.
func runServe(configPath, port string) {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Resolve the listen address. Precedence: --port flag > PORT env > config.
	addr := cfg.Listen
	if v := os.Getenv("PORT"); v != "" {
		addr = "127.0.0.1:" + v
	}
	if port != "" {
		addr = "127.0.0.1:" + port
	}

	reg := cfg.Registry()
	log.Printf("providers enabled: %s", strings.Join(reg.Names(), ", "))

	// Build the sink chain. Stdout is always on; the SQLite ledger is added when
	// storage is enabled or when enforcement is on (enforcement needs the ledger
	// to answer spend queries).
	sinks := []telemetry.Sink{telemetry.NewStdoutSink()}
	var st *store.Store
	if cfg.StorageEnabled() || cfg.Enforcement.Enabled {
		path := cfg.Storage.Path
		if path == "" {
			p, err := store.DefaultPath()
			if err != nil {
				log.Fatalf("store path: %v", err)
			}
			path = p
		}
		opened, err := store.Open(path)
		if err != nil {
			log.Fatalf("store open %s: %v", path, err)
		}
		st = opened
		defer func() { _ = st.Close() }()
		sinks = append(sinks, st)
	}

	// Optional OpenTelemetry exporter Sink. Flushed on shutdown below.
	var otelExp *otelexport.Exporter
	if cfg.OTelEnabled() {
		exp, err := otelexport.New(context.Background(), otelexport.Config{
			Endpoint: cfg.OTel.Endpoint,
			Insecure: cfg.OTelInsecure(),
			Headers:  cfg.OTel.Headers,
			Version:  Version,
		})
		if err != nil {
			log.Fatalf("otel exporter: %v", err)
		}
		otelExp = exp
		sinks = append(sinks, exp)
		log.Printf("otel: exporting spans via OTLP/HTTP")
	}

	resolver := buildPriceResolver(cfg)

	sink := telemetry.MultiSink(sinks...)
	rp := proxy.New(reg, sink, proxy.Options{
		DeveloperID: cfg.Developer.ID,
		Team:        cfg.Developer.Team,
		Pricing:     resolver,
	})

	var handler http.Handler = rp
	if cfg.Enforcement.Enabled {
		limits, err := cfg.EnforceLimits()
		if err != nil {
			log.Fatalf("enforcement limits: %v", err)
		}
		eng := enforce.NewEngine(limits, st.Spend, time.Now)
		resolve := func(r *http.Request) (string, string) {
			return identity.Resolve(r, cfg.Developer.ID, cfg.Developer.Team)
		}
		handler = enforce.Middleware(eng, resolve, reg, cfg.EnforceAction(), cfg.BlockStatus(), cfg.DowngradeRules())(handler)
	}
	// Reject undetected traffic outermost, before enforcement or metering, so a
	// stray/non-inference request 404s instead of being spend-checked or dialed.
	handler = proxy.Gate(reg)(handler)

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Cancel the context on SIGINT/SIGTERM to trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (inferenced %s)", addr, Version)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
		// Flush any buffered OTel spans before exit.
		if otelExp != nil {
			if err := otelExp.Shutdown(shutdownCtx); err != nil {
				log.Printf("otel shutdown failed: %v", err)
			}
		}
	}
}

// buildPriceResolver assembles the layered price resolver: a custom sheet (if
// configured) has priority, otherwise the live models.dev sheet is fetched on
// boot; the embedded defaults are always the final fallback. A custom sheet
// deliberately suppresses the live fetch, matching "pull prices live unless a
// custom sheet is present". Live-fetch failures degrade to the embedded
// defaults and never block startup; a misconfigured custom sheet is fatal.
func buildPriceResolver(cfg *config.Config) *pricing.Resolver {
	var layers []*pricing.Sheet

	custom := cfg.Pricing.Sheet != ""
	if custom {
		s, err := pricing.LoadFile(cfg.Pricing.Sheet)
		if err != nil {
			log.Fatalf("pricing sheet: %v", err)
		}
		layers = append(layers, s)
		log.Printf("pricing: using custom sheet %s (live fetch skipped)", cfg.Pricing.Sheet)
	}

	if !custom && cfg.RemoteEnabled() {
		ttl, err := cfg.RemoteTTL()
		if err != nil {
			log.Fatalf("pricing: %v", err)
		}
		fetchCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		sheet, err := pricing.LoadRemote(fetchCtx, pricing.RemoteOptions{
			URL:       cfg.Pricing.Remote.URL,
			TTL:       ttl,
			CachePath: cfg.Pricing.Remote.CachePath,
		})
		cancel()
		switch {
		case err != nil:
			log.Printf("pricing: live prices unavailable (%v); using embedded defaults", err)
		case sheet != nil:
			layers = append(layers, sheet)
			log.Printf("pricing: loaded live prices from models.dev")
		}
	}

	layers = append(layers, pricing.Default())
	return pricing.NewResolver(layers...)
}
