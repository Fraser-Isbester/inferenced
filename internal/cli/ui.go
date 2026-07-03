package cli

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fraser-Isbester/inferenced/internal/config"
	"github.com/Fraser-Isbester/inferenced/internal/store"
	"github.com/Fraser-Isbester/inferenced/internal/uiserver"
)

// defaultUIPort is the port the local web dashboard listens on (distinct from
// the gateway's own listen port).
const defaultUIPort = 8788

// init registers the ui command.
func init() { register(newUICmd) }

// newUICmd builds the "ui" command, which serves the read-only local dashboard
// (spend summaries + a live request feed) by reading the SQLite ledger.
func newUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Serve the local web dashboard (spend + live requests)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			configPath, _ := cmd.Flags().GetString("config")
			port, _ := cmd.Flags().GetInt("port")
			return runUI(configPath, port)
		},
	}
	cmd.Flags().Int("port", defaultUIPort, "TCP port the dashboard listens on")
	return cmd
}

// runUI opens the ledger read handle, builds the dashboard handler, and serves
// it on 127.0.0.1:<port> until interrupted.
func runUI(configPath string, port int) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	path := cfg.Storage.Path
	if path == "" {
		p, err := store.DefaultPath()
		if err != nil {
			return fmt.Errorf("store path: %w", err)
		}
		path = p
	}
	st, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", path, err)
	}
	defer func() { _ = st.Close() }()

	// The editor and gauges read/write the resolved config file; use the same
	// resolution as `serve` so both agree on which file to watch and edit.
	cfgPath := resolveConfigPath(configPath)

	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           uiserver.Handler(st, cfgPath),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("dashboard on http://%s (ledger: %s)", addr, path)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("ui server: %w", err)
	case <-ctx.Done():
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
		}
	}
	return nil
}
