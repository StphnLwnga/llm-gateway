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

// apiError pairs an internal error with the exact message the client sees.
// The handler writes the public message; the internal chain stays for
// classify (errors.Is on the sentinel) and for the log.
type apiError struct {
	err error
	msg string
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

// publicError returns err with msg as the only text the client sees.
func publicError(err error, msg string) error {
	return &apiError{err: err, msg: msg}
}

// classify maps an error to its HTTP status and OpenAI error type.
// An error it does not know is a 500.
func classify(err error) (status int, errType string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "invalid_request_error"
	case errors.Is(err, ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, "invalid_request_error"
	default:
		return http.StatusInternalServerError, "server_error"
	}
}

// serverErrorMessage is the only text a client sees for a 5xx. The real error
// can hold internal detail (hosts, SQL, upstream bodies), so it goes to the log.
const serverErrorMessage = "The server had an error while processing your request."

// writeError writes err in the OpenAI error shape. The client message is the
// one set with publicError, never err.Error(): a wrapped chain can carry
// internal detail. A 4xx with no public message gets the status text.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, errType := classify(err)
	msg := http.StatusText(status)
	var ae *apiError
	if errors.As(err, &ae) {
		msg = ae.msg
	}
	if status >= http.StatusInternalServerError {
		log.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "err", err)
		msg = serverErrorMessage
	}
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: msg, Type: errType}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) // the status is already sent; a failed write means the client left
}
