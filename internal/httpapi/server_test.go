package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
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
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body does not match the error envelope: %v", err)
	}
	return env.Error
}

func TestHealthzIsAlways200(t *testing.T) {
	t.Parallel()
	failing := checkFunc(func(context.Context) error { return errors.New("db down") })
	h := New(discard, nil, failing)
	h.StartDraining()

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
			h := New(discard, nil, tc.checks...)
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
	h := New(discard, nil, checkFunc(func(ctx context.Context) error {
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
	rec := do(t, New(discard, nil), http.MethodGet, "/v1/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"object":"list","data":[]}` {
		t.Errorf("body = %s, want {\"object\":\"list\",\"data\":[]}", got)
	}
}

func TestUnknownRouteIs404InTheOpenAIShape(t *testing.T) {
	t.Parallel()
	rec := do(t, New(discard, nil), http.MethodGet, "/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/nope = %d, want 404", rec.Code)
	}
	decodeError(t, rec)
	// OpenAI's own body for an unknown URL, with the method and path swapped in.
	want := `{"error":{"message":"Invalid URL (GET /v1/nope)","type":"invalid_request_error","param":null,"code":null}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestWrongMethodIs405InTheOpenAIShape(t *testing.T) {
	t.Parallel()
	rec := do(t, New(discard, nil), http.MethodPost, "/v1/models")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/models = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
	body := decodeError(t, rec)
	if body.Message != "Not allowed to POST on /v1/models. Use GET." || body.Type != "invalid_request_error" || body.Code != nil {
		t.Errorf("error = %+v, want the method message, type invalid_request_error, code null", body)
	}
}

func TestClientMessageNeverComesFromTheErrorChain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"public message set", publicError(fmt.Errorf("route lookup in table v3: %w", ErrNotFound), "Invalid URL (GET /x)"), "Invalid URL (GET /x)"},
		{"4xx without a public message", fmt.Errorf("route lookup in table v3: %w", ErrNotFound), "Not Found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeError(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil), discard, tc.err)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404: classify must still see the sentinel", rec.Code)
			}
			if body := decodeError(t, rec); body.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", body.Message, tc.wantMsg)
			}
		})
	}
}

func TestServerErrorHidesTheInternalText(t *testing.T) {
	t.Parallel()
	internal := errors.New("dial tcp 10.0.0.7:5432: connection refused")
	cases := []struct {
		name string
		err  error
	}{
		{"plain internal error", internal},
		{"internal error with a public message", publicError(internal, "database 10.0.0.7 is down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logged strings.Builder
			log := slog.New(slog.NewTextHandler(&logged, nil))
			rec := httptest.NewRecorder()
			writeError(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", nil), log, tc.err)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "10.0.0.7") {
				t.Errorf("the 500 body leaked internal detail: %s", rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Type != "server_error" || body.Message != serverErrorMessage || body.Code != nil {
				t.Errorf("error = %+v, want server_error, the fixed message, code null", body)
			}
			if !strings.Contains(logged.String(), "dial tcp 10.0.0.7:5432") {
				t.Errorf("the internal chain was not logged: %s", logged.String())
			}
		})
	}
}

// TestServerTimeouts pins the timeouts, and that WriteTimeout stays off.
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

type fixedModels []Model

func (f fixedModels) Models() []Model { return f }

func TestModelsListsTheRoutedAliasesInTheOpenAIShape(t *testing.T) {
	t.Parallel()
	h := New(discard, fixedModels{
		{ID: "chat", Created: time.Unix(1760000000, 0)},
		{ID: "fast", Created: time.Unix(1760000100, 0)},
	})
	rec := do(t, h, http.MethodGet, "/v1/models")
	want := `{"object":"list","data":[{"id":"chat","object":"model","created":1760000000,"owned_by":"llm-gateway"},` +
		`{"id":"fast","object":"model","created":1760000100,"owned_by":"llm-gateway"}]}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("GET /v1/models = %d %s, want 200 %s", rec.Code, rec.Body.String(), want)
	}
}
