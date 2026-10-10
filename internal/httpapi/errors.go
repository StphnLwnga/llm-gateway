package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Sentinel errors a handler returns. Only this file maps them to a status code.
var (
	ErrNotFound         = errors.New("not found")
	ErrMethodNotAllowed = errors.New("method not allowed")
)

// errorEnvelope is the OpenAI error shape. Every error response uses it, so an
// OpenAI SDK pointed at the gateway parses gateway errors like OpenAI's own.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// classify maps an error to its HTTP status, OpenAI error type and code.
// An error it does not know is a 500.
func classify(err error) (status int, errType, code string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "invalid_request_error", "not_found"
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed"
	default:
		return http.StatusInternalServerError, "server_error", "internal_error"
	}
}

// writeError writes err in the OpenAI error shape. A 4xx message is written for
// the client, so it is sent as is. A 5xx message can hold internal detail
// (hosts, SQL, upstream bodies), so the client gets a fixed text and the log gets err.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, errType, code := classify(err)
	msg := err.Error()
	if status >= http.StatusInternalServerError {
		log.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "err", err)
		msg = "The server had an error while processing your request."
	}
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: msg, Type: errType, Code: &code}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // the status is already sent; a failed write means the client left
}
