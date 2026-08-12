// Package session implements opaque cookie-based sessions and synchronizer-
// token CSRF protection. See ARCHITECTURE.md for why an opaque server-side
// session (not a JWT) was chosen and why CSRF uses a synchronizer token
// rather than a signed double-submit cookie — neither needs a signing
// secret, so there is no SESSION_SECRET env var.
package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/idgen"
)

const (
	CookieName     = "wp_session"
	CSRFHeaderName = "X-CSRF-Token"
)

var (
	ErrNoSession      = errors.New("session: no session cookie")
	ErrSessionExpired = errors.New("session: expired")
	ErrCSRFMismatch   = errors.New("session: csrf token mismatch")
)

type Manager struct {
	store       *dbx.Store
	idleTimeout time.Duration
	maxAge      time.Duration
}

func NewManager(store *dbx.Store, idleTimeout, maxAge time.Duration) *Manager {
	return &Manager{store: store, idleTimeout: idleTimeout, maxAge: maxAge}
}

// Create establishes a new session for userID, sets the session cookie on
// the response, and returns the created session (whose CSRFToken the caller
// should hand to the client, e.g. embedded in the post-login page).
func (m *Manager) Create(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) (*dbx.Session, error) {
	id, err := idgen.SessionID()
	if err != nil {
		return nil, err
	}
	csrfToken, err := idgen.SessionID() // same 256-bit random generator; distinct value/purpose from the session id
	if err != nil {
		return nil, err
	}
	now := time.Now()
	sess := dbx.Session{
		ID:         id,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(m.maxAge),
		CSRFToken:  csrfToken,
	}
	if err := m.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	m.setCookie(w, r, id, m.maxAge)
	return &sess, nil
}

// isSecureRequest reports whether r arrived over what the browser will
// treat as an HTTPS context — required to know per-request, not per-
// deployment, since a single Watch Party instance can legitimately be
// reachable at multiple origins with different schemes (e.g. an external
// HTTPS domain and an internal HTTP-only LAN hostname). A browser will
// simply refuse to store a Secure-flagged cookie set from a plain HTTP
// page, so getting this wrong for the HTTP origin breaks login silently;
// getting it wrong for the HTTPS origin weakens it for no reason — so it
// has to be derived from the actual request, not a single global flag.
//
// r.TLS is set when this process terminates TLS itself; behind a reverse
// proxy (the documented deployment — see ARCHITECTURE.md) it won't be, so
// X-Forwarded-Proto is trusted instead, which is the standard signal every
// reverse proxy (including Traefik) sets for exactly this purpose. This
// trust is safe within the documented architecture: the app is meant to be
// reachable only through that proxy (e.g. watchparty.container's example
// binds PublishPort to 127.0.0.1), not directly from untrusted clients who
// could otherwise forge the header.
func isSecureRequest(r *http.Request) bool {
	if r != nil && r.TLS != nil {
		return true
	}
	if r == nil {
		return false
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (m *Manager) setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func (m *Manager) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// Authenticate reads the session cookie from r, validates it against idle
// timeout and absolute max age, and — if valid — advances last_seen_at
// (sliding idle timeout) and returns the session. Expired sessions are
// deleted server-side before returning ErrSessionExpired.
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*dbx.Session, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, ErrNoSession
	}
	sess, err := m.store.GetSession(ctx, c.Value)
	if err != nil {
		if errors.Is(err, dbx.ErrNotFound) {
			return nil, ErrNoSession
		}
		return nil, err
	}
	now := time.Now()
	if now.After(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) > m.idleTimeout {
		_ = m.store.DeleteSession(ctx, sess.ID)
		return nil, ErrSessionExpired
	}
	if err := m.store.TouchSession(ctx, sess.ID, now); err != nil {
		return nil, err
	}
	sess.LastSeenAt = now
	return sess, nil
}

// Logout deletes the session server-side and clears the cookie. r is
// needed for the same reason as Create: a browser won't apply a
// Secure-flagged cookie deletion instruction from a plain HTTP page, so
// the clearing Set-Cookie has to match what scheme this particular request
// actually arrived over.
func (m *Manager) Logout(ctx context.Context, w http.ResponseWriter, r *http.Request, sessionID string) error {
	m.clearCookie(w, r)
	return m.store.DeleteSession(ctx, sessionID)
}

// ValidateCSRF checks the X-CSRF-Token request header against the token
// bound to sess at creation. Constant-time comparison avoids leaking the
// token's value through response-time side channels.
func ValidateCSRF(r *http.Request, sess *dbx.Session) error {
	got := r.Header.Get(CSRFHeaderName)
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
		return ErrCSRFMismatch
	}
	return nil
}
