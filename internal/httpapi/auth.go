package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/beechfuzz/watch-party/internal/emby"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type loginResponse struct {
	User      userResponse `json:"user"`
	CSRFToken string       `json:"csrf_token"`
}

// handleLogin proxies credentials to Emby and never stores the plaintext
// password — see spec "Authentication & Security". The password local
// variable goes out of scope (and is eligible for GC) as soon as this
// function returns; nothing here persists it anywhere.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "username and password are required")
		return
	}

	result, err := a.Emby.AuthenticateByName(r.Context(), req.Username, req.Password)
	if err != nil {
		if errors.Is(err, emby.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "invalid username or password")
			return
		}
		a.Logger.Error("emby authentication failed", "error", err)
		writeError(w, http.StatusBadGateway, "emby_unreachable", "could not reach the Emby server")
		return
	}

	encToken, err := a.TokenCipher.Encrypt(result.AccessToken)
	if err != nil {
		a.Logger.Error("token encryption failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if err := a.Store.UpsertUser(r.Context(), result.UserID, result.DisplayName, encToken, time.Now()); err != nil {
		a.Logger.Error("upsert user failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	sess, err := a.Sessions.Create(r.Context(), w, r, result.UserID)
	if err != nil {
		a.Logger.Error("create session failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	writeJSON(w, http.StatusOK, loginResponse{
		User:      userResponse{ID: result.UserID, DisplayName: result.DisplayName},
		CSRFToken: sess.CSRFToken,
	})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	if err := a.Sessions.Logout(r.Context(), w, r, sess.ID); err != nil {
		a.Logger.Error("logout failed", "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	user := userFromContext(r.Context())
	writeJSON(w, http.StatusOK, loginResponse{
		User:      userResponse{ID: user.ID, DisplayName: user.DisplayName},
		CSRFToken: sess.CSRFToken,
	})
}
