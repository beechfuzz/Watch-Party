package httpapi

import (
	"log/slog"
	"net/http"
)

// RegisterSetupRequiredRoutes attaches the temporary "please finish setup"
// placeholder used when /data/config/config.jsonc does not exist yet (see
// config.FileExists / cmd/server/main.go's runSetupRequired).
//
// TEMPORARY: this is a stand-in for the setup wizard UI, which is built in
// a later session. Every route (any path, any method, except /healthz)
// returns the same placeholder response. Do not extend this file with
// wizard logic — it should be replaced wholesale once the wizard lands,
// not grown incrementally.
//
// In normal mode (config.jsonc exists), this function is never called, so
// there is nothing to disable: any hypothetical setup-only path 404s via
// http.ServeMux's ordinary "no matching pattern" behavior.
func RegisterSetupRequiredRoutes(mux *http.ServeMux, logger *slog.Logger) {
	mux.HandleFunc("GET /healthz", Healthz) // liveness only; unchanged semantics in both modes

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable) // alive, but not configured/ready
		w.Write([]byte("Watch Party setup required: no configuration file found at /data/config/config.jsonc.\nSetup is not available in this build.\n"))
	})
}
