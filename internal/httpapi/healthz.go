package httpapi

import "net/http"

// Healthz reports liveness for container orchestration. It intentionally
// does not check DB connectivity or Emby reachability — a degraded backend
// dependency shouldn't cause the orchestrator to kill and restart a process
// that is otherwise fine and would just reconnect.
func Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
