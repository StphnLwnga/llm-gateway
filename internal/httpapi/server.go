// Package httpapi is the gateway's HTTP edge.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Checker is one readiness check.
type Checker interface {
	Check(ctx context.Context) error
}

// Model is one entry of GET /v1/models.
type Model struct {
	ID      string
	Created time.Time
}

// ModelLister lists the models the gateway can serve now.
type ModelLister interface {
	Models() []Model
}

const checkTimeout = 2 * time.Second

// Handler serves every gateway route.
type Handler struct {
	log      *slog.Logger
	models   ModelLister
	checks   []Checker
	draining atomic.Bool
	mux      *http.ServeMux
}

// New returns the gateway's handler. models may be nil, which lists nothing;
// checks run on every GET /readyz.
func New(log *slog.Logger, models ModelLister, checks ...Checker) *Handler {
	h := &Handler{log: log, models: models, checks: checks, mux: http.NewServeMux()}
	for _, rt := range h.routes() {
		h.mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
		// Without this, ServeMux answers a wrong method with a plain-text 405.
		h.mux.HandleFunc(rt.path, h.methodNotAllowed(rt.method))
	}
	h.mux.HandleFunc("/", h.notFound)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// StartDraining makes GET /readyz return 503.
func (h *Handler) StartDraining() {
	h.draining.Store(true)
}

// Routes lists every route as "METHOD /path".
func (h *Handler) Routes() []string {
	var out []string
	for _, rt := range h.routes() {
		out = append(out, rt.method+" "+rt.path)
	}
	return out
}

type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

func (h *Handler) routes() []route {
	return []route{
		{http.MethodGet, "/healthz", h.healthz},
		{http.MethodGet, "/readyz", h.readyz},
		{http.MethodGet, "/v1/models", h.listModels},
	}
}

// healthz never checks a dependency: a database outage must not restart every pod.
func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	for _, c := range h.checks {
		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		err := c.Check(ctx)
		cancel()
		if err != nil {
			h.log.WarnContext(r.Context(), "readiness check failed", "check", fmt.Sprintf("%T", c), "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type modelList struct {
	Object string  `json:"object"`
	Data   []model `json:"data"`
}

type model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *Handler) listModels(w http.ResponseWriter, _ *http.Request) {
	out := modelList{Object: "list", Data: []model{}}
	if h.models != nil {
		for _, m := range h.models.Models() {
			out.Data = append(out.Data, model{ID: m.ID, Object: "model", Created: m.Created.Unix(), OwnedBy: "llm-gateway"})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, h.log, publicError(ErrNotFound, fmt.Sprintf("Invalid URL (%s %s)", r.Method, r.URL.Path)))
}

func (h *Handler) methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, r, h.log, publicError(ErrMethodNotAllowed,
			fmt.Sprintf("Not allowed to %s on %s. Use %s.", r.Method, r.URL.Path, allow)))
	}
}

// NewServer returns the HTTP server, with timeouts against slow and idle clients.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: it would cut off every streamed response. S1 sets a
		// deadline per event instead.
	}
}
