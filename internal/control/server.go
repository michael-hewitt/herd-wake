package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Provider supplies the daemon-side data and lifecycle operations served by
// the control API. The daemon implements it.
type Provider interface {
	Status() StatusResponse
	// StartProject starts the named project and returns once it is running
	// (readiness succeeded) or startup failed.
	StartProject(ctx context.Context, name string) (ProjectStatus, error)
	// StopProject gracefully stops the named project's process group.
	StopProject(ctx context.Context, name string) (ProjectStatus, error)
	// RestartProject stops (if needed) and starts the named project.
	RestartProject(ctx context.Context, name string) (ProjectStatus, error)
	// LeaseProject marks the named project active for ttl, parking its idle
	// countdown until the lease expires or is released. A new lease replaces
	// any existing one.
	LeaseProject(ctx context.Context, name string, ttl time.Duration) (ProjectStatus, error)
	// ReleaseProjectLease clears the named project's activity lease.
	ReleaseProjectLease(ctx context.Context, name string) (ProjectStatus, error)
	// ProjectLogs returns up to maxLines recent output lines (all buffered
	// lines when maxLines <= 0).
	ProjectLogs(name string, maxLines int) (LogsResponse, error)
}

// NewHandler returns the HTTP handler the daemon serves on the control
// socket.
func NewHandler(provider Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, provider.Status())
	})

	action := func(pattern string, do func(ctx context.Context, name string) (ProjectStatus, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			status, err := do(r.Context(), r.PathValue("name"))
			if err != nil {
				writeError(w, errorStatusCode(err), err)
				return
			}
			writeJSON(w, status)
		})
	}
	action("POST /v1/projects/{name}/start", provider.StartProject)
	action("POST /v1/projects/{name}/stop", provider.StopProject)
	action("POST /v1/projects/{name}/restart", provider.RestartProject)
	action("DELETE /v1/projects/{name}/lease", provider.ReleaseProjectLease)

	mux.HandleFunc("POST /v1/projects/{name}/lease", func(w http.ResponseWriter, r *http.Request) {
		ttl, err := time.ParseDuration(r.URL.Query().Get("ttl"))
		if err != nil || ttl <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("ttl must be a positive Go duration, e.g. ttl=30m"))
			return
		}
		status, err := provider.LeaseProject(r.Context(), r.PathValue("name"), ttl)
		if err != nil {
			writeError(w, errorStatusCode(err), err)
			return
		}
		writeJSON(w, status)
	})

	mux.HandleFunc("GET /v1/projects/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		maxLines := 0
		if v := r.URL.Query().Get("lines"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				writeError(w, http.StatusBadRequest, errors.New("lines must be a non-negative integer"))
				return
			}
			maxLines = n
		}
		logs, err := provider.ProjectLogs(r.PathValue("name"), maxLines)
		if err != nil {
			writeError(w, errorStatusCode(err), err)
			return
		}
		writeJSON(w, logs)
	})

	return mux
}

// errorStatusCode maps a provider error to an HTTP status code.
func errorStatusCode(err error) int {
	if errors.Is(err, ErrUnknownProject) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(errorResponse{Error: err.Error()}); encErr != nil {
		// Headers are already written; nothing useful left to do.
		_ = encErr
	}
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
