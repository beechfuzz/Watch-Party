package httpapi

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/beechfuzz/watch-party/internal/config"
	"github.com/beechfuzz/watch-party/internal/idgen"
	"github.com/beechfuzz/watch-party/internal/session"
	"github.com/beechfuzz/watch-party/internal/webassets"
)

// This file replaces internal/httpapi/setup_placeholder.go wholesale, per
// that file's own doc comment ("should be replaced wholesale once the
// wizard lands, not grown incrementally"): RegisterSetupWizardRoutes now
// owns everything setup_placeholder.go used to (GET /healthz, the 503
// catch-all fallback for every other path) plus the wizard form itself at
// GET/POST /. RegisterAwaitingRestartRoutes, at the bottom of this file,
// is the third and last placeholder mux this project needs -- see
// cmd/server/main.go's runAwaitingRestart.

const setupCSRFCookieName = "wp_setup_csrf"

// setupCSRFCookieMaxAge is generous on purpose: it only needs to outlive
// however long an operator takes to read the generated key, fill in 15
// fields, and click submit -- not a normal session lifetime.
const setupCSRFCookieMaxAge = 10 * time.Minute

// generatedKey holds the operator-facing encryption key this wizard
// generates and displays once. It is deliberately its own type, not a
// plain string field on setupWizard, so the only code that can ever read
// its value is code that explicitly asks for a generatedKey -- most
// importantly, config.WriteFile's signature takes a *config.FileConfig,
// which structurally has no field for this value (see FileConfig's own
// doc comment), so there is no call site anywhere that could pass this
// through even by accident. NEVER log this value, at any level, and
// NEVER thread it into anything that ends up inside config.jsonc -- it
// exists only to be rendered into the HTML response body, once, for the
// operator to copy into their own environment. See CLAUDE.md's
// credential-logging invariant and FileConfig's doc comment for why.
type generatedKey struct{ value string }

// DisplayValue is the only accessor -- named to make every call site read
// as "this is going on a page for a human to read", not "this is a value
// safe to pass around programmatically".
func (k generatedKey) DisplayValue() string { return k.value }

// setupWizard holds the wizard's per-process state: the configPath it
// will write to, the channel it signals on a successful write, and the
// encryption key it generated once at RegisterSetupWizardRoutes time and
// displays on every subsequent render for the rest of this process's
// setup-required lifetime (regenerating it per-request would make an
// operator who reloads the page unsure which copy is the "real" one).
type setupWizard struct {
	logger        *slog.Logger
	configPath    string
	configWritten chan<- struct{}
	key           generatedKey
}

// setupPageData is what setup.html executes against. Deliberately not
// pageData (pages.go) -- the two share essentially no fields, and
// overloading pageData with wizard-only concerns (Values/Errors/the
// generated key) would make it harder to reason about what normal-mode
// pages actually use.
type setupPageData struct {
	Success      bool
	Values       map[string]string
	Errors       map[string]string
	FormError    string
	CSRFToken    string
	GeneratedKey string
}

// RegisterSetupWizardRoutes attaches the setup wizard to mux: GET /
// renders the form, POST / validates and writes configPath, GET /healthz
// is unchanged liveness-only, and everything else still 503s exactly as
// setup_placeholder.go's placeholder used to. configWritten, if non-nil,
// receives a non-blocking signal as the last thing a successful POST
// does, after its HTTP response has been fully written -- cmd/server's
// run loop uses this to know when to stop serving this mux and attempt a
// transition to normal mode (see runSetupRequired).
//
// No authentication is required to reach any of this, consistent with
// this project's existing trust model (see CLAUDE.md/ARCHITECTURE.md):
// Watch Party is not meant to be directly internet-reachable. State-
// changing access (the POST) is still protected -- by the CSRF double-
// submit cookie below, not by auth, since setup-required mode has no
// session/user concept yet to authenticate against.
func RegisterSetupWizardRoutes(mux *http.ServeMux, logger *slog.Logger, configPath string, configWritten chan<- struct{}) error {
	keyVal, err := config.GenerateKey()
	if err != nil {
		return fmt.Errorf("setup wizard: generating encryption key: %w", err)
	}

	sw := &setupWizard{
		logger:        logger,
		configPath:    configPath,
		configWritten: configWritten,
		key:           generatedKey{value: keyVal},
	}

	mux.HandleFunc("GET /healthz", Healthz)
	mux.HandleFunc("GET /", sw.handleGet)
	mux.HandleFunc("POST /", sw.handlePost)
	// The bare "/" pattern below is a subtree match (per net/http's
	// ServeMux: a pattern ending in "/" matches that path and everything
	// under it, unless a more specific pattern also matches) -- so is
	// "GET /" above, restricted to the GET method. Neither is an exact
	// match for the literal root: absent any registered pattern for
	// "/party/abc" or similar, "GET /" would otherwise catch every GET
	// request to any path, method-restricted-subtree beating method-
	// unrestricted-subtree. handleGet/handlePost each guard against this
	// explicitly (matching the same pattern pages.go's own "GET /" home
	// handler already uses for the identical reason), falling through to
	// this same 503 for anything other than the literal "/".
	mux.HandleFunc("/", serveSetupUnavailable)

	staticSub, err := webassets.StaticFS()
	if err != nil {
		return fmt.Errorf("setup wizard: webassets static fs: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(http.FileServer(http.FS(staticSub)))))

	return nil
}

// RegisterAwaitingRestartRoutes attaches the placeholder served once the
// wizard has already written configPath but this process couldn't
// transition into normal mode in-process, because TOKEN_ENCRYPTION_KEY/
// _FILE still isn't set in the environment -- an env var can't be
// injected into an already-running process, so only a genuine operator-
// initiated restart (after they set it) can finish the job; see
// cmd/server/main.go's runAwaitingRestart for the full reasoning.
//
// No wizard *form* routes are registered here -- config.jsonc already
// exists, so re-running the wizard against it must be structurally
// impossible, the same "different mux, nothing to disable" pattern this
// project already uses for setup-required vs. normal mode. GET /static/ is
// the one exception, registered identically to the other two muxes
// (setup-required's and normal mode's -- see setup_wizard.go's own
// RegisterSetupWizardRoutes and pages.go's RegisterRoutes): the setup
// wizard's own POST / response -- the "Configuration saved" confirmation
// page the operator is looking at right now, served by the *previous*
// mux just before this one took over -- links this mux's /static/css/
// style.css, and without this route every load of that already-rendered
// page's stylesheet 503s for as long as this placeholder is up, which is
// however long it takes the operator to notice, set the key, and restart
// (see ARCHITECTURE.md §16.25 -- found and fixed alongside the unrelated
// listener-handoff race that motivated that section, not because this
// path is racy itself; it isn't, it's simply never served here at all).
func RegisterAwaitingRestartRoutes(mux *http.ServeMux, logger *slog.Logger) error {
	mux.HandleFunc("GET /healthz", Healthz)

	staticSub, err := webassets.StaticFS()
	if err != nil {
		return fmt.Errorf("awaiting-restart placeholder: webassets static fs: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(http.FileServer(http.FS(staticSub)))))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Watch Party: configuration saved. Set TOKEN_ENCRYPTION_KEY or TOKEN_ENCRYPTION_KEY_FILE in the environment and restart the server to finish setup.\n"))
	})
	return nil
}

// serveSetupUnavailable is the 503 fallback for every path/method the
// wizard doesn't explicitly handle -- registered directly against "/" for
// every method other than GET/POST, and called directly by
// handleGet/handlePost for any GET/POST whose path isn't the literal
// root (see RegisterSetupWizardRoutes's comment on why that guard is
// needed at all).
func serveSetupUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte("Watch Party setup required: visit this address in a browser to finish configuration.\n"))
}

func (sw *setupWizard) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		serveSetupUnavailable(w, r)
		return
	}
	csrfToken, err := sw.issueCSRFCookie(w, r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	renderPage(w, sw.logger, pageTemplates, "setup.html", setupPageData{
		Values:       defaultSetupFieldValues(),
		Errors:       map[string]string{},
		CSRFToken:    csrfToken,
		GeneratedKey: sw.key.DisplayValue(),
	})
}

func (sw *setupWizard) handlePost(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		serveSetupUnavailable(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	if err := sw.validateCSRF(r); err != nil {
		http.Error(w, "missing or invalid CSRF token; reload the page and try again", http.StatusForbidden)
		return
	}

	fc, values, fieldErrs := sw.buildFileConfig(r)

	if len(fieldErrs) > 0 {
		sw.reRenderWithErrors(w, r, values, fieldErrs, "")
		return
	}

	if err := config.WriteFile(sw.configPath, fc); err != nil {
		sw.logger.Error("setup wizard: write config.jsonc", "error", err)
		sw.reRenderWithErrors(w, r, values, map[string]string{}, "Could not save the configuration. Check the server logs and try again.")
		return
	}

	renderPage(w, sw.logger, pageTemplates, "setup.html", setupPageData{
		Success:      true,
		GeneratedKey: sw.key.DisplayValue(),
	})

	// Signaled only after the response above has been fully written --
	// http.Server.Shutdown (which cmd/server's run loop calls once it
	// receives this) waits for this same handler invocation to return
	// before closing anything, so the confirmation page this request is
	// receiving is never cut off by the mux swap that follows.
	if sw.configWritten != nil {
		select {
		case sw.configWritten <- struct{}{}:
		default:
		}
	}
}

// reRenderWithErrors re-renders the form with the operator's submitted
// values and per-field errors preserved, plus a fresh CSRF cookie/token
// pair (the one that was just spent either validated or didn't -- either
// way a new one is simplest and correct for the next submission attempt).
func (sw *setupWizard) reRenderWithErrors(w http.ResponseWriter, r *http.Request, values, fieldErrs map[string]string, formError string) {
	csrfToken, err := sw.issueCSRFCookie(w, r)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	renderPage(w, sw.logger, pageTemplates, "setup.html", setupPageData{
		Values:       values,
		Errors:       fieldErrs,
		FormError:    formError,
		CSRFToken:    csrfToken,
		GeneratedKey: sw.key.DisplayValue(),
	})
}

func (sw *setupWizard) issueCSRFCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	token, err := idgen.SessionID() // same 256-bit random generator session.Manager uses for its own CSRF tokens; distinct value/purpose here
	if err != nil {
		sw.logger.Error("setup wizard: generate csrf token", "error", err)
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     setupCSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   session.IsSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(setupCSRFCookieMaxAge.Seconds()),
	})
	return token, nil
}

// validateCSRF implements the double-submit-cookie check: the cookie set
// on the GET that rendered this form must match the hidden csrf_token
// field the operator's browser submits back. Neither side depends on any
// server-side session storage -- setup-required mode has none (see
// ARCHITECTURE.md §16) -- the cookie itself is the storage. A cross-origin
// attacker can't read the cookie's value (Same-Origin Policy) to forge a
// matching form field, and SameSite=Strict additionally stops the cookie
// from even being attached to a cross-site request in the first place.
func (sw *setupWizard) validateCSRF(r *http.Request) error {
	cookie, err := r.Cookie(setupCSRFCookieName)
	if err != nil || cookie.Value == "" {
		return fmt.Errorf("missing csrf cookie")
	}
	submitted := r.FormValue("csrf_token")
	if submitted == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(submitted)) != 1 {
		return fmt.Errorf("csrf token mismatch")
	}
	return nil
}

// defaultSetupFieldValues returns the pre-filled defaults shown on a
// fresh GET /. Every field except browser_origins/server_url/public_url
// matches FileConfig's documented defaults (see internal/config/config.go's
// loadFrom and README.md's config.jsonc example) field-for-field.
//
// browser_origins/server_url/public_url are the deliberate exception: an
// operator decision (not a loadFrom match -- see ARCHITECTURE.md §16.20) to
// give these three a real, non-blank wizard-only suggested value instead of
// shipping blank, so a fresh install always lands on Step 1 instead of
// being routed straight to Step 2 by wizard-steps.js's required-field gate.
// internal/config.loadFrom itself is unchanged and still has no fallback
// for either browser_origins or server_url -- these values are reused
// as-is from the verified design source's own illustrative example data
// (its initial component state and README-documented "Default" for these
// fields), not invented here, and an operator who never touches them and
// submits anyway will get exactly these values written to config.jsonc:
// syntactically valid, so nothing rejects them, but almost certainly wrong
// for their specific deployment.
func defaultSetupFieldValues() map[string]string {
	return map[string]string{
		"title":                    "Watch Party",
		"log_level":                "info",
		"browser_origins":          "https://watchparty.example.com",
		"listen_address":           ":8080",
		"session_idle_timeout":     "24h",
		"session_age_timeout":      "720h",
		"host_grace_period":        "20s",
		"inactivity_timeout":       "48h",
		"progress_interval":        "10s",
		"sync_snapshot_interval":   "4s",
		"sync_soft_drift":          "300ms",
		"sync_hard_drift":          "1500ms",
		"sync_max_rate_adjustment": "0.05",
		"server_url":               "http://emby:8096",
		"public_url":               "http://emby:8096",
	}
}

// buildFileConfig validates every submitted field using the same
// exported validators internal/config's own loader uses (see config.go),
// so there is exactly one implementation of each rule. It always returns
// all three of: the FileConfig built so far (fields that failed
// validation are left nil), the raw submitted values (for re-rendering
// the form on failure), and a map of field-name -> error message (empty
// if every field passed).
func (sw *setupWizard) buildFileConfig(r *http.Request) (*config.FileConfig, map[string]string, map[string]string) {
	values := map[string]string{}
	fieldErrs := map[string]string{}
	fc := &config.FileConfig{}

	get := func(name string) string {
		v := strings.TrimSpace(r.FormValue(name))
		values[name] = v
		return v
	}

	title := get("title")
	if err := config.ValidateTitle(title); err != nil {
		fieldErrs["title"] = err.Error()
	} else {
		fc.ServerSettings.Title = &title
	}

	logLevel := get("log_level")
	if err := config.ValidateLogLevel(logLevel); err != nil {
		fieldErrs["log_level"] = err.Error()
	} else {
		fc.ServerSettings.LogLevel = &logLevel
	}

	originsRaw := r.FormValue("browser_origins") // preserve exactly as typed (newlines) for re-render, unlike the trimmed get()
	values["browser_origins"] = originsRaw
	if origins, err := config.ValidateOrigins(splitOriginsInput(originsRaw)); err != nil {
		fieldErrs["browser_origins"] = err.Error()
	} else {
		fc.ServerSettings.BrowserOrigins = &origins
	}

	listenAddress := get("listen_address")
	if err := config.ValidateListenAddress(listenAddress); err != nil {
		fieldErrs["listen_address"] = err.Error()
	} else {
		fc.ServerSettings.ListenAddress = &listenAddress
	}

	for _, lenient := range []struct {
		field string
		dst   **string
	}{
		{"session_idle_timeout", &fc.ServerSettings.SessionIdleTimeout},
		{"session_age_timeout", &fc.ServerSettings.SessionAgeTimeout},
		{"host_grace_period", &fc.GlobalPartySettings.HostGracePeriod},
		{"inactivity_timeout", &fc.GlobalPartySettings.InactivityTimeout},
		{"progress_interval", &fc.GlobalPlaybackSettings.ProgressInterval},
		{"sync_snapshot_interval", &fc.GlobalPlaybackSettings.SyncSnapshotInterval},
	} {
		v := get(lenient.field)
		if _, err := config.ParseDuration(v); err != nil {
			fieldErrs[lenient.field] = err.Error()
		} else {
			*lenient.dst = &v
		}
	}

	softRaw := get("sync_soft_drift")
	hardRaw := get("sync_hard_drift")
	softD, softErr := config.ParseDurationStrict(softRaw)
	if softErr != nil {
		fieldErrs["sync_soft_drift"] = softErr.Error()
	}
	hardD, hardErr := config.ParseDurationStrict(hardRaw)
	if hardErr != nil {
		fieldErrs["sync_hard_drift"] = hardErr.Error()
	}
	if softErr == nil && hardErr == nil {
		if err := config.ValidateSyncDrift(softD, hardD); err != nil {
			fieldErrs["sync_hard_drift"] = err.Error()
		} else {
			fc.GlobalPlaybackSettings.SyncSoftDrift = &softRaw
			fc.GlobalPlaybackSettings.SyncHardDrift = &hardRaw
		}
	}

	rateRaw := get("sync_max_rate_adjustment")
	if rate, err := strconv.ParseFloat(rateRaw, 64); err != nil {
		fieldErrs["sync_max_rate_adjustment"] = fmt.Sprintf("invalid number %q", rateRaw)
	} else if err := config.ValidateMaxRateAdjustment(rate); err != nil {
		fieldErrs["sync_max_rate_adjustment"] = err.Error()
	} else {
		fc.GlobalPlaybackSettings.SyncMaxRateAdjustment = &rate
	}

	serverURL := get("server_url")
	if serverURL == "" {
		fieldErrs["server_url"] = "is required"
	} else {
		fc.MediaServerSettings.ServerURL = &serverURL
	}

	publicURL := get("public_url") // optional: blank is a valid, meaningful value (see FileConfig's doc comment)
	fc.MediaServerSettings.PublicURL = &publicURL

	return fc, values, fieldErrs
}

// splitOriginsInput splits on newlines and/or commas, since the wizard's
// textarea accepts either style; config.ValidateOrigins does the actual
// trimming/empty-filtering/minimum-count validation on the result.
func splitOriginsInput(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })
}
