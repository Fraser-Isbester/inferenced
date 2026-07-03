// Command gateway runs the inferenced local reverse proxy. It listens on
// 127.0.0.1 and intercepts LLM inference traffic for the registered providers,
// forwarding upstream while emitting structured telemetry.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Fraser-Isbester/inferenced/internal/provider"
	"github.com/Fraser-Isbester/inferenced/internal/provider/anthropic"
	"github.com/Fraser-Isbester/inferenced/internal/provider/openai"
	"github.com/Fraser-Isbester/inferenced/internal/proxy"
	"github.com/Fraser-Isbester/inferenced/internal/telemetry"
)

func main() {
	// Default port, overridable by --port flag or the PORT environment variable.
	defaultPort := "8766"
	if v := os.Getenv("PORT"); v != "" {
		defaultPort = v
	}
	port := flag.String("port", defaultPort, "TCP port to listen on (127.0.0.1)")
	flag.Parse()

	reg := provider.NewRegistry()
	reg.Register(anthropic.New())
	reg.Register(openai.New())

	rp := proxy.New(reg, telemetry.NewStdoutSink())

	addr := "127.0.0.1:" + *port
	srv := &http.Server{
		Addr:    addr,
		Handler: rp,
	}

	// Cancel the context on SIGINT/SIGTERM to trigger graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
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
