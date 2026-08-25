// Command server is the Watch Party binary: an HTTP + WebSocket server that
// coordinates synchronized Emby playback across multiple browsers. See
// ARCHITECTURE.md for the design and README.md for setup instructions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/beechfuzz/watch-party/internal/privdrop"
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

// run dispatches between the two startup modes based solely on whether
// config.DefaultConfigPath exists -- there is no separate
// "enable_setup_wizard" flag. If it doesn't exist, the server starts in
// setup-required mode: a minimal, dependency-free placeholder server (see
// runSetupRequired) that doesn't open the database, construct the token
// cipher, or require TOKEN_ENCRYPTION_KEY/_FILE to be set at all. If it
// exists, the server starts normally (runNormal), which is the only path
// that resolves and validates the encryption key.
func run() error {
	exists, err := config.FileExists(config.DefaultConfigPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if !exists {
		return runSetupRequired()
	}
	return runNormal()
}

// runSetupRequired serves the temporary "please finish setup" placeholder
// (see httpapi.RegisterSetupRequiredRoutes) when config.DefaultConfigPath
// doesn't exist. It deliberately does the minimum possible: resolve just
// enough config to bind a listener and pick a log level, and nothing else
// -- no database, no privilege drop, no Emby client, no party hub, and no
// TOKEN_ENCRYPTION_KEY/_FILE requirement, since none of those have valid
// inputs yet and requiring the key here would block the setup wizard (a
// later phase) from ever being reachable to tell an operator about it.
func runSetupRequired() error {
	listenAddr, err := config.ResolveListenAddress(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	logLevel, err := config.ResolveLogLevel(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := logging.New(logLevel)
	logger.Info("starting watch party in setup-required mode",
		"listen_addr", listenAddr,
		"reason", "no config file found at "+config.DefaultConfigPath,
	)

	mux := http.NewServeMux()
	httpapi.RegisterSetupRequiredRoutes(mux, logger)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serveUntilShutdown(srv, logger, nil)
}

// runNormal is today's startup path, driven by the full env+file config
// merge (config.Load). This is the only path that resolves and validates
// TOKEN_ENCRYPTION_KEY/TOKEN_ENCRYPTION_KEY_FILE.
func runNormal() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := logging.New(cfg.LogLevel)
	logger.Info("starting watch party", "listen_addr", cfg.ListenAddr, "app_origins", cfg.AppOrigins, "title", cfg.Title)
	logTokenKeySource(logger)
	if nonHTTPS := cfg.NonHTTPSOrigins(); len(nonHTTPS) > 0 {
		logger.Warn("APP_ORIGINS includes non-HTTPS origin(s); session cookies issued for these will not be marked Secure, matching what browsers require for a plain HTTP page — make sure they're only reachable on a trusted network", "origins", nonHTTPS)
	}

	dataDir := dirOf(cfg.DatabasePath)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("creating database directory: %w", err)
	}

	// No-op unless actually running as root (the container image starts
	// as root specifically to make this possible — see Dockerfile); takes
	// ownership of the data directory and permanently drops to PUID:PGID
	// before opening the database or listening on anything. See
	// internal/privdrop and ARCHITECTURE.md.
	if os.Geteuid() == 0 && (cfg.PUID == 0 || cfg.PGID == 0) {
		logger.Warn("PUID or PGID resolved to 0 (root); the server will continue running as root", "puid", cfg.PUID, "pgid", cfg.PGID)
	}
	if err := privdrop.Apply(privdrop.Config{UID: cfg.PUID, GID: cfg.PGID, Paths: []string{dataDir}}); err != nil {
		return fmt.Errorf("dropping privileges to PUID=%d PGID=%d: %w", cfg.PUID, cfg.PGID, err)
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

	sessions := session.NewManager(store, cfg.SessionIdleTimeout, cfg.SessionMaxAge)
	embyClient := emby.NewClient(cfg.EmbyServerURL)
	embyClient.SetPublicBaseURL(cfg.EmbyPublicURL)

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

	sweepCtx, stopSweep := context.WithCancel(context.Background())
	defer stopSweep()
	go runPartyInactivitySweep(sweepCtx, hub, cfg.PartyInactivityTimeout, logger)

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
		Title:                cfg.Title,
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serveUntilShutdown(srv, logger, func(shutdownCtx context.Context) {
		hub.Shutdown(shutdownCtx)
	})
}

// logTokenKeySource logs which of TOKEN_ENCRYPTION_KEY /
// TOKEN_ENCRYPTION_KEY_FILE supplied the encryption key -- and, when it
// was the file variant, the file's path -- never the key value or file
// contents. By the time this is called, config.Load has already
// succeeded, so exactly one of the two is guaranteed to be set.
func logTokenKeySource(logger *slog.Logger) {
	if os.Getenv("TOKEN_ENCRYPTION_KEY") != "" {
		logger.Info("token encryption key loaded", "source", "env")
		return
	}
	logger.Info("token encryption key loaded", "source", "file", "path", os.Getenv("TOKEN_ENCRYPTION_KEY_FILE"))
}

// serveUntilShutdown starts srv, blocks until a SIGTERM/SIGINT or a fatal
// listen error, then drains it with a bounded shutdown timeout. onShutdown,
// if non-nil, runs after the HTTP server itself has stopped accepting new
// requests but before "shutdown complete" is logged -- runNormal uses it to
// flush the party hub's in-memory state to SQLite; runSetupRequired has
// nothing to flush, so it passes nil.
func serveUntilShutdown(srv *http.Server, logger *slog.Logger, onShutdown func(ctx context.Context)) error {
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
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

	if onShutdown != nil {
		onShutdown(shutdownCtx)
	}

	logger.Info("shutdown complete")
	return nil
}

// partyInactivitySweepInterval is how often to check for inactive parties,
// not the inactivity threshold itself (that's cfg.PartyInactivityTimeout,
// operator-configurable and 48h by default) -- 15 minutes of imprecision on
// a multi-hour timeout is immaterial, so this isn't exposed as its own
// setting.
const partyInactivitySweepInterval = 15 * time.Minute

// runPartyInactivitySweep periodically ends parties idle for longer than
// maxIdle until ctx is cancelled (server shutdown). See
// party.Hub.SweepInactiveParties and ARCHITECTURE.md §8.
func runPartyInactivitySweep(ctx context.Context, hub *party.Hub, maxIdle time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(partyInactivitySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := hub.SweepInactiveParties(ctx, maxIdle); n > 0 {
				logger.Info("party inactivity sweep ended parties", "count", n)
			}
		}
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
