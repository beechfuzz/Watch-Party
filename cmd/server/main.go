// Command server is the Watch Party binary: an HTTP + WebSocket server that
// coordinates synchronized Emby playback across multiple browsers. See
// ARCHITECTURE.md for the design and README.md for setup instructions.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/beechfuzz/watch-party/internal/config"
	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/embyreport"
	"github.com/beechfuzz/watch-party/internal/httpapi"
	"github.com/beechfuzz/watch-party/internal/logging"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/session"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--generate-key" {
		key, err := config.GenerateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error generating key:", err)
			os.Exit(1)
		}
		fmt.Println(key)
		return
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := logging.New(cfg.LogLevel)
	logger.Info("starting watch party", "listen_addr", cfg.ListenAddr, "dev_mode", cfg.DevMode)

	if err := os.MkdirAll(dirOf(cfg.DatabasePath), 0o755); err != nil {
		return fmt.Errorf("creating database directory: %w", err)
	}
	db, err := dbx.Open(cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()
	store := dbx.NewStore(db)

	tokenCipher, err := cryptox.NewTokenCipher(cfg.TokenEncryptionKey)
	if err != nil {
		return fmt.Errorf("token cipher: %w", err)
	}

	sessions := session.NewManager(store, cfg.SessionIdleTimeout, cfg.SessionMaxAge, cfg.DevMode)
	embyClient := emby.NewClient(cfg.EmbyServerURL)

	hub := party.NewHub(store, party.Tuning{
		SnapshotInterval:  cfg.SyncSnapshotInterval,
		SoftDriftMS:       cfg.SyncSoftDriftMS,
		HardDriftMS:       cfg.SyncHardDriftMS,
		MaxRateAdjustment: cfg.SyncMaxRateAdjust,
		HostGracePeriod:   cfg.HostGracePeriod,
	}, logger)

	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer recoverCancel()
	if err := hub.RecoverActiveParties(recoverCtx); err != nil {
		return fmt.Errorf("recovering active parties: %w", err)
	}

	reporter := embyreport.New(hub, store, embyClient, tokenCipher, cfg.EmbyProgressInterval, logger)
	reporterCtx, stopReporter := context.WithCancel(context.Background())
	defer stopReporter()
	go reporter.Run(reporterCtx)

	mux := http.NewServeMux()
	httpapi.RegisterRoutes(mux, &httpapi.App{
		Store:                store,
		Sessions:             sessions,
		Hub:                  hub,
		Emby:                 embyClient,
		TokenCipher:          tokenCipher,
		Reporter:             reporter,
		Logger:               logger,
		AppOrigins:           cfg.AppOrigins,
		EmbyProgressInterval: cfg.EmbyProgressInterval,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	hub.Shutdown(shutdownCtx)

	logger.Info("shutdown complete")
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
