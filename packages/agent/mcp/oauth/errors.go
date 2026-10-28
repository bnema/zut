package oauth

import "errors"

var errCredentials = errors.New("OAuth credentials unavailable or rejected")

// operationError preserves machine-readable causes without displaying URLs,
// response bodies, or credentials carried by an underlying error.
type operationError struct {
	message string
	cause   error
}

func (e *operationError) Error() string           { return e.message }
func (e *operationError) Unwrap() error           { return e.cause }
func safeError(message string, cause error) error { return &operationError{message, cause} }
