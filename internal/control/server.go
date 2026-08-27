package control

import (
	"encoding/json"
	"net/http"
)

// StatusProvider supplies the daemon-side data served by the control API.
// The daemon implements it; later slices extend this interface (or add
// siblings) for lifecycle commands.
type StatusProvider interface {
	Status() StatusResponse
}

// NewHandler returns the HTTP handler the daemon serves on the control
// socket.
func NewHandler(provider StatusProvider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, provider.Status())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// Headers are already written; nothing useful left to do.
		_ = err
	}
}
