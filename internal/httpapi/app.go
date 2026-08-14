// Package httpapi wires the HTTP + WebSocket surface: authentication,
// party management, and the real-time sync channel. Handlers are thin —
// they translate HTTP/WS requests into calls on the party/session/emby
// packages, which hold the actual logic.
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/embyreport"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/session"
)

// App bundles every dependency HTTP/WS handlers need.
type App struct {
	Store       *dbx.Store
	Sessions    *session.Manager
	Hub         *party.Hub
	Emby        *emby.Client
	TokenCipher *cryptox.TokenCipher
	Reporter    *embyreport.Reporter
	Logger      *slog.Logger

	AppOrigins []string

	EmbyProgressInterval time.Duration
}

// RegisterRoutes attaches every Watch Party HTTP/WS route to mux.
func RegisterRoutes(mux *http.ServeMux, app *App) {
	mux.HandleFunc("GET /healthz", Healthz)

	mux.HandleFunc("POST /api/auth/login", app.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", app.withAuth(app.withCSRF(app.handleLogout)))
	mux.HandleFunc("GET /api/me", app.withAuth(app.handleMe))

	mux.HandleFunc("POST /api/parties", app.withAuth(app.withCSRF(app.handleCreateParty)))
	mux.HandleFunc("GET /api/parties", app.withAuth(app.handleListParties))
	mux.HandleFunc("GET /api/parties/{id}", app.withAuth(app.handleGetParty))
	mux.HandleFunc("GET /api/parties/{id}/playback-url", app.withAuth(app.handlePlaybackURL))
	mux.HandleFunc("POST /api/parties/{id}/leave", app.withAuth(app.withCSRF(app.handleLeaveParty)))
	mux.HandleFunc("POST /api/parties/{id}/end", app.withAuth(app.withCSRF(app.handleEndParty)))
	mux.HandleFunc("POST /api/parties/{id}/host-transfer", app.withAuth(app.withCSRF(app.handleHostTransfer)))

	mux.HandleFunc("GET /ws/parties/{id}", app.handleWebSocket) // auth + origin validated inside (cookie-based; see ws.go)

	registerPages(mux)
}
