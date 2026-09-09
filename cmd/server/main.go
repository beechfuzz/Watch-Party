// Command server is the Watch Party binary: an HTTP + WebSocket server that
// coordinates synchronized Emby playback across multiple browsers. See
// ARCHITECTURE.md for the design and README.md for setup instructions.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// run builds the single OS-signal-derived context this process's entire
// lifetime uses for graceful shutdown -- constructed exactly once here,
// never inside any of the run* functions below, because Go's signal
// handling is process-global: installing a second signal.NotifyContext
// mid-process (which runLoop's incarnations otherwise would, once per
// transition) would be redundant at best and is also what would make the
// loop untestable, since a test driving runLoop directly needs to supply
// its own cancellable context instead of sending real OS signals (which
// would hit the whole test binary, not just the code under test) -- see
// cmd/server/run_test.go.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return runLoop(ctx, config.DefaultConfigPath, nil)
}

// runOutcome distinguishes why runSetupRequired stopped serving.
type runOutcome int

const (
	outcomeShutdown runOutcome = iota
	outcomeConfigWritten
)

// runLoop is run()'s actual logic, extracted so cmd/server/run_test.go can
// drive it directly against a real temporary configPath and a test-
// controlled ctx, instead of the hardcoded config.DefaultConfigPath and
// real OS signals main() uses. listening, if non-nil, receives the actual
// bound address every time acquireListener performs a real bind -- nil in
// production (main always binds the configured LISTEN_ADDR and has no need
// to observe it), non-nil in tests using LISTEN_ADDR=127.0.0.1:0 to learn
// which ephemeral port was actually chosen, without sleeping or polling.
//
// Three incarnations can run in this process's lifetime, one at a time,
// each on the same listen address in the common case, none of them ever
// exiting the process to transition to the next. acquireListener (the
// closure below) reuses one persistent connBroker across a transition
// whenever the desired address is unchanged from the previous incarnation
// (see listener.go and ARCHITECTURE.md §16.25): the real OS socket is
// never closed and rebound on that path, which is what closes the race
// that used to exist between one incarnation's listener closing and the
// next one's binding -- a request arriving in that window (e.g. the setup
// wizard's own confirmation page fetching its stylesheet right after the
// automatic transition to normal mode) used to get refused outright; now
// it simply waits until the next incarnation's http.Server starts Serve.
// A change in the desired address (operator-edited listen_address with no
// LISTEN_ADDR env override) or an explicitly ephemeral ":0" request still
// closes the old socket for real and binds a fresh one, same as before.
//
//  1. runSetupRequired, whenever configPath doesn't exist yet. Ends either
//     because ctx was cancelled (a real shutdown -- runLoop returns nil,
//     same as today) or because the setup wizard wrote configPath (loop
//     back around and re-check).
//  2. Once configPath exists, an attempt to load it in full
//     (config.LoadFromPath, the same normal-mode entrypoint as before).
//     If that fails specifically because TOKEN_ENCRYPTION_KEY/_FILE isn't
//     set -- config.ErrTokenEncryptionKeyRequired, distinguishable from
//     any other config problem via errors.Is -- runAwaitingRestart takes
//     over: environment variables can't be injected into an already-
//     running process, so only a genuine operator-initiated restart (after
//     they set the key) can make progress from here; this process waits
//     inertly rather than crash-looping or silently stalling. Any other
//     load failure is a hard error, as it always was.
//  3. Otherwise, runNormalWithConfig -- today's full startup path,
//     unchanged, just parameterized by the already-loaded *config.Config
//     and the shared ctx instead of loading it again and building its own
//     signal context.
func runLoop(ctx context.Context, configPath string, listening chan<- string) error {
	// One connBroker is reused across every incarnation that shares the
	// same listen address (see listener.go and ARCHITECTURE.md §16.25) --
	// acquireListener below only tears it down for real (via realClose)
	// when the desired address actually changes, or when addr is an
	// ephemeral ":0"-style request, which always means "give me a new
	// port" rather than "reuse the last one." This defer is what closes it
	// for real when runLoop itself returns (a genuine process shutdown),
	// regardless of which branch got there.
	var broker *connBroker
	var brokerAddr string
	defer func() {
		if broker != nil {
			broker.realClose()
		}
	}()

	// acquireListener only decides which underlying socket the next
	// incarnation will use (reuse the existing broker, or really bind a
	// new one) -- it deliberately does NOT send on listening itself. That
	// send happens later, inside serveUntilShutdown/
	// serveSetupRequiredUntilDone, at the exact point (right before
	// srv.Serve starts) the pre-listener-reuse code used to send it: for
	// runNormalWithConfig specifically, that's *after* privdrop/DB-open/
	// hub-construction have already succeeded, which existing tests rely
	// on (a value on listening has always meant "this incarnation's setup
	// work is done, not just that a socket exists") -- see
	// TestRunLoop_KeyAlreadySet_TransitionsInProcessWithoutExiting's
	// cgo-privdrop skip path, which depends on privdrop failures
	// surfacing via `result` before anything ever arrives on `listening`.
	acquireListener := func(addr string) (*handoff, error) {
		if broker == nil || brokerAddr != addr || isEphemeralAddr(addr) {
			if broker != nil {
				broker.realClose()
			}
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return nil, fmt.Errorf("listen on %q: %w", addr, err)
			}
			broker = newConnBroker(l)
			brokerAddr = addr
		}
		return broker.newHandoff(), nil
	}

	for {
		exists, err := config.FileExists(configPath)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}

		if !exists {
			addr, err := config.ResolveListenAddress(nil)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			l, err := acquireListener(addr)
			if err != nil {
				return err
			}
			outcome, err := runSetupRequired(ctx, configPath, l, listening)
			if err != nil {
				return err
			}
			if outcome == outcomeShutdown {
				return nil
			}
			continue
		}

		cfg, cfgErr := config.LoadFromPath(configPath)
		if cfgErr != nil && errors.Is(cfgErr, config.ErrTokenEncryptionKeyRequired) {
			addr, err := config.ResolveListenAddress(nil)
			if err != nil {
				return fmt.Errorf("config: %w", err)
			}
			l, err := acquireListener(addr)
			if err != nil {
				return err
			}
			return runAwaitingRestart(ctx, l, listening)
		}
		if cfgErr != nil {
			return fmt.Errorf("config: %w", cfgErr)
		}
		l, err := acquireListener(cfg.ListenAddr)
		if err != nil {
			return err
		}
		return runNormalWithConfig(ctx, cfg, l, listening)
	}
}

// runSetupRequired serves the setup wizard (see
// httpapi.RegisterSetupWizardRoutes) when configPath doesn't exist. It
// deliberately does the minimum possible beyond the wizard itself: pick a
// log level -- no database, no privilege drop, no Emby client, no party
// hub, and no TOKEN_ENCRYPTION_KEY/_FILE requirement, since none of those
// have valid inputs yet and requiring the key here would block the wizard
// from ever being reachable to tell an operator about it. l is already
// bound and, in the common case, will keep being reused by the next
// incarnation this process transitions to -- see runLoop's acquireListener
// and ARCHITECTURE.md §16.25. listening is passed straight through to
// serveSetupRequiredUntilDone -- see its doc comment for why the send
// happens there rather than at acquisition time.
func runSetupRequired(ctx context.Context, configPath string, l net.Listener, listening chan<- string) (runOutcome, error) {
	logLevel, err := config.ResolveLogLevel(nil)
	if err != nil {
		return outcomeShutdown, fmt.Errorf("config: %w", err)
	}

	logger := logging.New(logLevel)
	logger.Info("starting watch party in setup-required mode",
		"listen_addr", l.Addr().String(),
		"reason", "no config file found at "+configPath,
	)

	configWritten := make(chan struct{}, 1)

	mux := http.NewServeMux()
	if err := httpapi.RegisterSetupWizardRoutes(mux, logger, configPath, configWritten); err != nil {
		return outcomeShutdown, fmt.Errorf("setup wizard: %w", err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serveSetupRequiredUntilDone(ctx, srv, l, logger, configWritten, listening)
}

// runAwaitingRestart serves the minimal "configuration saved, restart me"
// placeholder (see httpapi.RegisterAwaitingRestartRoutes) once
// config.jsonc exists but this process couldn't transition into normal
// mode in-process because TOKEN_ENCRYPTION_KEY/_FILE isn't set. It never
// exits on its own -- only ctx cancellation (a real shutdown signal) ends
// it -- specifically so this process never crash-loops or relies on any
// orchestrator's restart-on-exit-code behavior; see runLoop's doc comment
// and ARCHITECTURE.md §16 for the Compose-vs-quadlet asymmetry this
// sidesteps entirely by never exiting as part of this transition. l is
// already bound -- see runSetupRequired's doc comment. listening is passed
// straight through to serveUntilShutdown.
func runAwaitingRestart(ctx context.Context, l net.Listener, listening chan<- string) error {
	logLevel, err := config.ResolveLogLevel(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := logging.New(logLevel)
	logger.Info("configuration saved but TOKEN_ENCRYPTION_KEY/TOKEN_ENCRYPTION_KEY_FILE is not set; waiting for a manual restart with the key set",
		"listen_addr", l.Addr().String(),
	)

	mux := http.NewServeMux()
	if err := httpapi.RegisterAwaitingRestartRoutes(mux, logger); err != nil {
		return fmt.Errorf("awaiting-restart placeholder: %w", err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serveUntilShutdown(ctx, srv, l, logger, nil, listening)
}

// runNormalWithConfig is today's startup path, driven by an already-loaded
// *config.Config (runLoop calls config.LoadFromPath itself, once, so this
// function doesn't load it again) and the shared ctx (see run()'s doc
// comment for why it's shared rather than constructed here). l is already
// bound -- see runSetupRequired's doc comment. listening is passed straight
// through to serveUntilShutdown, which sends on it only once this
// function's own setup work (privilege drop, DB open, hub construction)
// has already succeeded -- see runLoop's acquireListener doc comment for
// why that ordering matters to existing tests.
func runNormalWithConfig(ctx context.Context, cfg *config.Config, l net.Listener, listening chan<- string) error {
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
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return serveUntilShutdown(ctx, srv, l, logger, func(shutdownCtx context.Context) {
		hub.Shutdown(shutdownCtx)
	}, listening)
}

// logTokenKeySource logs which of TOKEN_ENCRYPTION_KEY /
// TOKEN_ENCRYPTION_KEY_FILE supplied the encryption key -- and, when it
// was the file variant, the file's path -- never the key value or file
// contents. By the time this is called, config.LoadFromPath has already
// succeeded, so exactly one of the two is guaranteed to be set.
func logTokenKeySource(logger *slog.Logger) {
	if os.Getenv("TOKEN_ENCRYPTION_KEY") != "" {
		logger.Info("token encryption key loaded", "source", "env")
		return
	}
	logger.Info("token encryption key loaded", "source", "file", "path", os.Getenv("TOKEN_ENCRYPTION_KEY_FILE"))
}

// serveUntilShutdown serves on the already-bound l (see runLoop's
// acquireListener -- l is typically a *handoff sharing a persistent
// connBroker with whatever incarnation preceded this one, not a
// freshly-bound listener; see ARCHITECTURE.md §16.25), blocks until ctx is
// cancelled or a fatal serve error occurs, then drains srv with a bounded
// shutdown timeout. onShutdown, if non-nil, runs after the HTTP server
// itself has stopped accepting new requests but before "shutdown complete"
// is logged -- runNormalWithConfig uses it to flush the party hub's
// in-memory state to SQLite; runSetupRequired/runAwaitingRestart have
// nothing to flush, so they pass nil (runSetupRequired doesn't use this
// function directly at all -- see serveSetupRequiredUntilDone, which
// duplicates this function's serve/drain shape specifically because it
// needs a third, config-written select case this one doesn't).
//
// listening, if non-nil, receives l's address right here, immediately
// before srv.Serve starts -- deliberately not any earlier (e.g. at
// listener-acquisition time in runLoop), because for runNormalWithConfig
// this is the point after privilege drop, DB open, and hub construction
// have already succeeded. Existing tests depend on that ordering: a value
// on listening has always meant "this incarnation's setup work is done,"
// not merely "a socket exists" -- see
// TestRunLoop_KeyAlreadySet_TransitionsInProcessWithoutExiting's
// cgo-privdrop skip path.
func serveUntilShutdown(ctx context.Context, srv *http.Server, l net.Listener, logger *slog.Logger, onShutdown func(ctx context.Context), listening chan<- string) error {
	if listening != nil {
		listening <- l.Addr().String()
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", l.Addr().String())
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

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

// serveSetupRequiredUntilDone is serveUntilShutdown's shape plus a third
// select case: configWritten, fired by the setup wizard's POST handler
// right after it finishes writing configPath (see
// httpapi.RegisterSetupWizardRoutes). On that case, srv is drained exactly
// as on a real shutdown, but the returned outcome tells runLoop to loop
// back around and attempt a transition to normal mode instead of exiting
// the process. l is already bound -- see serveUntilShutdown's doc comment;
// the same reuse applies here, which is precisely what lets the setup
// wizard's own confirmation page (served by the incarnation this call
// belongs to) keep its static assets reachable through the handoff to
// whatever incarnation runLoop starts next. listening, if non-nil, is sent
// to synchronously, same as serveUntilShutdown.
func serveSetupRequiredUntilDone(ctx context.Context, srv *http.Server, l net.Listener, logger *slog.Logger, configWritten <-chan struct{}, listening chan<- string) (runOutcome, error) {
	if listening != nil {
		listening <- l.Addr().String()
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", l.Addr().String())
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	outcome := outcomeShutdown
	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	case <-configWritten:
		logger.Info("setup wizard wrote config.jsonc, attempting to switch to normal mode")
		outcome = outcomeConfigWritten
	case err := <-serveErr:
		if err != nil {
			return outcomeShutdown, fmt.Errorf("server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http server shutdown error", "error", err)
	}

	logger.Info("setup-required server stopped", "outcome", int(outcome))
	return outcome, nil
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
