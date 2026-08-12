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
	// devMode permits non-Secure cookies for local http:// testing (e.g.
	// http://192.168.x.x:8080). Never disable the Secure attribute outside
	// of this explicit opt-in.
	devMode bool
}

func NewManager(store *dbx.Store, idleTimeout, maxAge time.Duration, devMode bool) *Manager {
	return &Manager{store: store, idleTimeout: idleTimeout, maxAge: maxAge, devMode: devMode}
}

// Create establishes a new session for userID, sets the session cookie on
// the response, and returns the created session (whose CSRFToken the caller
// should hand to the client, e.g. embedded in the post-login page).
func (m *Manager) Create(ctx context.Context, w http.ResponseWriter, userID string) (*dbx.Session, error) {
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
	m.setCookie(w, id, m.maxAge)
	return &sess, nil
}

func (m *Manager) setCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   !m.devMode,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

func (m *Manager) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   !m.devMode,
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

// Logout deletes the session server-side and clears the cookie.
func (m *Manager) Logout(ctx context.Context, w http.ResponseWriter, sessionID string) error {
	m.clearCookie(w)
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
