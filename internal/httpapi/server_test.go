package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var discard = slog.New(slog.DiscardHandler)

// checkFunc adapts a function to the Checker interface.
type checkFunc func(context.Context) error

func (f checkFunc) Check(ctx context.Context) error { return f(ctx) }

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// decodeError checks the OpenAI error shape and returns its body.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, rec.Body.String())
	}
	for _, key := range []string{"message", "type", "param", "code"} {
		if _, ok := raw["error"][key]; !ok {
			t.Errorf("error object has no %q key: %s", key, rec.Body.String())
		}
	}
	var env errorEnvelope
	json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error
}

func TestHealthzIsAlways200(t *testing.T) {
	t.Parallel()
	failing := checkFunc(func(context.Context) error { return errors.New("db down") })
	h := New(discard, failing)
	h.StartDraining()

	// Liveness ignores dependencies and draining: restarting the pod fixes neither.
	if rec := do(t, h, http.MethodGet, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	t.Parallel()
	ok := checkFunc(func(context.Context) error { return nil })
	failing := checkFunc(func(context.Context) error { return errors.New("db down") })

	cases := []struct {
		name     string
		checks   []Checker
		draining bool
		want     int
	}{
		{"no checks", nil, false, http.StatusOK},
		{"all checks pass", []Checker{ok, ok}, false, http.StatusOK},
		{"one check fails", []Checker{ok, failing}, false, http.StatusServiceUnavailable},
		{"draining", []Checker{ok}, true, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := New(discard, tc.checks...)
			if tc.draining {
				h.StartDraining()
			}
			rec := do(t, h, http.MethodGet, "/readyz")
			if rec.Code != tc.want {
				t.Errorf("GET /readyz = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "db down") {
				t.Errorf("readyz leaked the check error: %s", rec.Body.String())
			}
		})
	}
}

func TestReadyzBoundsEachCheck(t *testing.T) {
	t.Parallel()
	var deadline time.Time
	h := New(discard, checkFunc(func(ctx context.Context) error {
		deadline, _ = ctx.Deadline()
		return nil
	}))
	do(t, h, http.MethodGet, "/readyz")
	if deadline.IsZero() {
		t.Error("the check's context has no deadline: a hung dependency would hang the probe")
	}
}

func TestModelsReturnsTheOpenAIListShape(t *testing.T) {
	t.Parallel()
	rec := do(t, New(discard), http.MethodGet, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"object":"list","data":[]}` {
		t.Errorf("body = %s, want {\"object\":\"list\",\"data\":[]}", got)
	}
}

func TestUnknownRouteIs404InTheOpenAIShape(t *testing.T) {
	t.Parallel()
	rec := do(t, New(discard), http.MethodGet, "/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/nope = %d, want 404", rec.Code)
	}
	body := decodeError(t, rec)
	if body.Type != "invalid_request_error" || body.Code == nil || *body.Code != "not_found" {
		t.Errorf("error = %+v, want type invalid_request_error, code not_found", body)
	}
	if !strings.Contains(body.Message, "/v1/nope") {
		t.Errorf("message %q does not name the path", body.Message)
	}
}

func TestWrongMethodIs405InTheOpenAIShape(t *testing.T) {
	t.Parallel()
	rec := do(t, New(discard), http.MethodPost, "/v1/models")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
	decodeError(t, rec)
}

func TestServerErrorHidesTheInternalText(t *testing.T) {
	t.Parallel()
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	internal := errors.New("dial tcp 10.0.0.7:5432: connection refused")

	rec := httptest.NewRecorder()
	writeError(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil), log, internal)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("the 500 body leaked the internal error: %s", rec.Body.String())
	}
	if body := decodeError(t, rec); body.Type != "server_error" {
		t.Errorf("type = %q, want server_error", body.Type)
	}
	if !strings.Contains(logged.String(), "10.0.0.7") {
		t.Errorf("the internal error was not logged: %s", logged.String())
	}
}

// TestServerTimeouts pins the server's timeout decisions. net/http implements the
// timeouts; this test makes sure they stay configured, and that WriteTimeout stays off.
func TestServerTimeouts(t *testing.T) {
	t.Parallel()
	srv := NewServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s: a slow client can hold a connection open (Slowloris)", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s: a slow request body can hold a connection open", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want 2m: idle kept-alive connections must be closed", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0: it would cut off every streamed answer", srv.WriteTimeout)
	}
}
