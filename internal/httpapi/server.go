// Package httpapi is the gateway's HTTP edge: routes, the OpenAI error shape,
// and the server's timeouts.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Checker is one readiness check, for example a Postgres ping (added in S0 c).
// It is defined here, where it is used, so a dependency's package does not import httpapi.
type Checker interface {
	Check(ctx context.Context) error
}

// checkTimeout bounds each readiness check, so a hung dependency fails the probe
// instead of hanging it.
const checkTimeout = 2 * time.Second

// Handler serves every gateway route.
type Handler struct {
	log      *slog.Logger
	checks   []Checker
	draining atomic.Bool
	mux      *http.ServeMux
}

// New returns the gateway's handler. checks run on every GET /readyz.
func New(log *slog.Logger, checks ...Checker) *Handler {
	h := &Handler{log: log, checks: checks, mux: http.NewServeMux()}
	for _, rt := range h.routes() {
		h.mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
		// The same path without a method catches every other method, so a wrong
		// method gets a 405 in the OpenAI shape, not ServeMux's plain-text one.
		h.mux.HandleFunc(rt.path, h.methodNotAllowed(rt.method))
	}
	h.mux.HandleFunc("/", h.notFound)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// StartDraining makes GET /readyz return 503, so a load balancer stops sending
// new requests. In-flight requests are not affected.
func (h *Handler) StartDraining() {
	h.draining.Store(true)
}

// route is one HTTP route: a method, a ServeMux path, and its handler.
type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

// routes lists every route the gateway serves.
func (h *Handler) routes() []route {
	return []route{
		{http.MethodGet, "/healthz", h.healthz},
		{http.MethodGet, "/readyz", h.readyz},
		{http.MethodGet, "/v1/models", h.models},
	}
}

// healthz is the liveness probe. It answers 200 while the process can serve
// HTTP. It never checks a dependency: a database outage must not make
// Kubernetes restart every gateway pod.
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is the readiness probe. It answers 503 while the server drains, or
// when a check fails, so the load balancer sends traffic to other pods.
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

// modelList is the OpenAI list shape for GET /v1/models.
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

// models is a stub until the router (S1) knows the model aliases.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelList{Object: "list", Data: []model{}})
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, h.log, fmt.Errorf("%w: invalid URL (%s %s)", ErrNotFound, r.Method, r.URL.Path))
}

func (h *Handler) methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, r, h.log, fmt.Errorf("%w: %s %s (allowed: %s)", ErrMethodNotAllowed, r.Method, r.URL.Path, allow))
	}
}

// NewServer returns the HTTP server, with timeouts against slow and idle clients.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,  // a client that sends headers slowly cannot hold a connection (Slowloris)
		ReadTimeout:       30 * time.Second, // the whole request, body included
		IdleTimeout:       2 * time.Minute,  // a kept-alive connection with no new request
		// No WriteTimeout: it covers the whole response, so it would cut off every
		// streamed answer. The streaming handler (S1) sets a deadline for each event instead.
	}
}
