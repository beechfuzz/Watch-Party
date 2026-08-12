package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/session"
)

type ctxKey int

const (
	ctxKeySession ctxKey = iota
	ctxKeyUser
)

func withSessionUser(ctx context.Context, sess *dbx.Session, user *dbx.User) context.Context {
	ctx = context.WithValue(ctx, ctxKeySession, sess)
	return context.WithValue(ctx, ctxKeyUser, user)
}

func sessionFromContext(ctx context.Context) *dbx.Session {
	s, _ := ctx.Value(ctxKeySession).(*dbx.Session)
	return s
}

func userFromContext(ctx context.Context) *dbx.User {
	u, _ := ctx.Value(ctxKeyUser).(*dbx.User)
	return u
}

// withAuth requires a valid session cookie, attaches the session and user
// to the request context, and rejects with 401 otherwise.
func (a *App) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := a.Sessions.Authenticate(r.Context(), r)
		if err != nil {
			if errors.Is(err, session.ErrNoSession) || errors.Is(err, session.ErrSessionExpired) {
				writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
				return
			}
			a.Logger.Error("session authenticate failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		user, err := a.Store.GetUser(r.Context(), sess.UserID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		}
		ctx := withSessionUser(r.Context(), sess, user)
		next(w, r.WithContext(ctx))
	}
}

// withCSRF requires the synchronizer token header to match the session's
// bound CSRF token, for state-changing requests. See ARCHITECTURE.md §1.4.
func (a *App) withCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		}
		if err := session.ValidateCSRF(r, sess); err != nil {
			writeError(w, http.StatusForbidden, "csrf_mismatch", "missing or invalid CSRF token")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
