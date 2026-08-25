package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newSetupRequiredMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	RegisterSetupRequiredRoutes(mux, slog.Default())
	return mux
}

func TestSetupRequired_Healthz_StaysOK(t *testing.T) {
	mux := newSetupRequiredMux(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "ok" {
		t.Errorf("GET /healthz body = %q, want \"ok\"", body)
	}
}

func TestSetupRequired_CatchAll_Returns503(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/party/abc"},
		{http.MethodPost, "/api/parties"},
		{http.MethodGet, "/static/js/foo.js"},
		{http.MethodGet, "/api/me"},
		{http.MethodDelete, "/api/parties/1/playlist/2"},
		{http.MethodGet, "/setup"},
		{http.MethodGet, "/ws/parties/1"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			mux := newSetupRequiredMux(t)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s status = %d, want 503", tc.method, tc.path, rec.Code)
			}
			body, _ := io.ReadAll(rec.Result().Body)
			if len(body) == 0 {
				t.Errorf("%s %s: empty placeholder body", tc.method, tc.path)
			}
		})
	}
}

// TestNormalMode_SetupOnlyPath_404s proves the other half of the
// mutual-exclusivity requirement: RegisterRoutes (the real, normal-mode
// router) never registers anything setup-related, so a hypothetical
// setup-style path 404s for free via http.ServeMux's default behavior --
// there is nothing to explicitly disable.
func TestNormalMode_SetupOnlyPath_404s(t *testing.T) {
	mux := http.NewServeMux()
	RegisterRoutes(mux, &App{Logger: slog.Default()})

	for _, path := range []string{"/setup", "/setup/", "/api/setup", "/wizard"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want 404 in normal mode", path, rec.Code)
			}
		})
	}
}
