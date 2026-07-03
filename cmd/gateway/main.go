// Command gateway runs the inferenced local reverse proxy. It listens on
// 127.0.0.1 and intercepts LLM inference traffic for the registered providers,
// forwarding upstream while emitting structured telemetry.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/enforce"
	"github.com/Fraser-Isbester/inferenced/internal/identity"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

// version is the build version, injected via -ldflags "-X main.version=..." by
// goreleaser. It defaults to "dev" for local builds.
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to config file (default: search ./inferenced.yaml and $XDG_CONFIG_HOME)")
	port := flag.String("port", "", "TCP port to listen on (overrides config listen address)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("inferenced", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Resolve the listen address. Precedence: --port flag > PORT env > config.
	addr := cfg.Listen
	if v := os.Getenv("PORT"); v != "" {
		addr = "127.0.0.1:" + v
	}
	if *port != "" {
		addr = "127.0.0.1:" + *port
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

	sink := telemetry.MultiSink(sinks...)
	rp := proxy.New(reg, sink, proxy.Options{
		DeveloperID: cfg.Developer.ID,
		Team:        cfg.Developer.Team,
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
		handler = enforce.Middleware(eng, resolve, reg, cfg.EnforceAction(), cfg.BlockStatus())(handler)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	// Cancel the context on SIGINT/SIGTERM to trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (inferenced %s)", addr, version)
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
	}
}
