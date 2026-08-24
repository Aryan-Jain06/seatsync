// Package httpx provides JSON response helpers and a typed API error that
// carries an HTTP status, so handlers can return errors and let one place
// decide how they are rendered.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// ErrorCode is a stable, machine-readable identifier for a failure. Clients
// branch on these rather than on human-readable messages.
type ErrorCode string

const (
	CodeBadRequest    ErrorCode = "bad_request"
	CodeValidation    ErrorCode = "validation_failed"
	CodeUnauthorized  ErrorCode = "unauthorized"
	CodeForbidden     ErrorCode = "forbidden"
	CodeNotFound      ErrorCode = "not_found"
	CodeConflict      ErrorCode = "conflict"
	CodeSeatsTaken    ErrorCode = "seats_unavailable"
	CodeHoldExpired   ErrorCode = "hold_expired"
	CodePaymentFailed ErrorCode = "payment_failed"
	CodeRateLimited   ErrorCode = "rate_limited"
	CodeInternal      ErrorCode = "internal_error"
)

// APIError is an error that knows how it should surface over HTTP.
type APIError struct {
	Status  int
	Code    ErrorCode
	Message string
	// Details carries structured context, for example the seat IDs that were
	// already taken when a hold failed.
	Details map[string]any
	// err is the underlying cause. It is logged but never sent to the client.
	err error
}

func (e *APIError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.err)
	}
	return e.Message
}

func (e *APIError) Unwrap() error { return e.err }

// WithDetails attaches structured context to the error.
func (e *APIError) WithDetails(details map[string]any) *APIError {
	e.Details = details
	return e
}

// Wrap attaches an underlying cause for logging.
func (e *APIError) Wrap(err error) *APIError {
	e.err = err
	return e
}

// Constructors for the failures the API actually returns.

func BadRequest(msg string) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: CodeBadRequest, Message: msg}
}

func Validation(msg string) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidation, Message: msg}
}

func Unauthorized(msg string) *APIError {
	return &APIError{Status: http.StatusUnauthorized, Code: CodeUnauthorized, Message: msg}
}

func Forbidden(msg string) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: CodeForbidden, Message: msg}
}

func NotFound(msg string) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: msg}
}

func Conflict(code ErrorCode, msg string) *APIError {
	return &APIError{Status: http.StatusConflict, Code: code, Message: msg}
}

func Internal(err error) *APIError {
	return &APIError{
		Status:  http.StatusInternalServerError,
		Code:    CodeInternal,
		Message: "Something went wrong on our side.",
		err:     err,
	}
}

// errorBody is the wire format for every failure.
type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// JSON writes v as a JSON response with the given status.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so the response cannot be salvaged.
		// Record it and move on rather than panicking in a request path.
		slog.Error("failed to encode response body", "error", err)
	}
}

// NoContent writes a bare 204.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error renders err as JSON. Unrecognised errors become a 500 and their
// details are logged rather than leaked to the caller.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internal(err)
	}

	if apiErr.Status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed",
			"error", apiErr.Error(),
			"method", r.Method,
			"path", r.URL.Path,
			"code", apiErr.Code,
		)
	}

	JSON(w, apiErr.Status, errorBody{Error: errorPayload{
		Code:    apiErr.Code,
		Message: apiErr.Message,
		Details: apiErr.Details,
	}})
}

// unknownFieldPrefix is how encoding/json reports a field rejected by
// DisallowUnknownFields. There is no typed error for this case, so the
// message is matched instead.
const unknownFieldPrefix = "json: unknown field "

// maxBodyBytes caps request bodies. Seat holds carry at most a handful of
// UUIDs, so anything larger is either a bug or an attack.
const maxBodyBytes = 1 << 20 // 1 MiB

// DecodeJSON reads and validates a JSON request body into dst.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError

		switch {
		case errors.Is(err, io.EOF):
			return BadRequest("Request body must not be empty.")
		case errors.As(err, &maxErr):
			return BadRequest("Request body is too large.")
		case errors.As(err, &syntaxErr):
			return BadRequest(fmt.Sprintf("Request body contains malformed JSON at position %d.", syntaxErr.Offset))
		case errors.As(err, &typeErr):
			return BadRequest(fmt.Sprintf("Field %q has the wrong type.", typeErr.Field))
		case strings.HasPrefix(err.Error(), unknownFieldPrefix):
			field := strings.Trim(strings.TrimPrefix(err.Error(), unknownFieldPrefix), `"`)
			return BadRequest(fmt.Sprintf("Request body contains an unrecognised field %q.", field))
		default:
			return BadRequest("Request body could not be parsed.").Wrap(err)
		}
	}

	// Reject trailing content so `{"a":1}{"b":2}` is not silently accepted.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BadRequest("Request body must contain exactly one JSON object.")
	}
	return nil
}

// StatusPaymentRequired is used for a declined payment: the request was well
// formed and the booking is valid, but the provider refused the charge.
const StatusPaymentRequired = http.StatusPaymentRequired
