// Package apperr defines the stable, machine-readable errors exposed by the CLI.
package apperr

import (
	"errors"
	"fmt"
)

// Stable error codes. Never rename; only add.
const (
	Invalid           = "INVALID_ARGUMENT"
	NotFound          = "NOT_FOUND"
	Conflict          = "CONFLICT"
	InvalidTransition = "INVALID_TRANSITION"
	Cycle             = "DEPENDENCY_CYCLE"
	NoProject         = "NO_PROJECT"
	Ambiguous         = "AMBIGUOUS_IDENTITY"
	ConfigInvalid     = "CONFIG_INVALID"
	SchemaTooNew      = "SCHEMA_TOO_NEW"
	Busy              = "DB_BUSY"
	RequestIDReused   = "REQUEST_ID_REUSED"
	Usage             = "USAGE"
	CheckFailed       = "CHECK_FAILED"
	Internal          = "INTERNAL"
	NoRun             = "NO_RUN"
	Unavailable       = "UNAVAILABLE" // a required external tool (tmux, git, harness) failed
	Forbidden         = "FORBIDDEN"   // a child acting for another run, or without its run token
	NotImplemented    = "NOT_IMPLEMENTED"
)

// Error is an error with a stable code.
type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string { return e.Message }

// New returns a non-retryable error with the given code.
func New(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Retryable returns a retryable error with the given code.
func Retryable(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: true}
}

// As extracts an *Error from err, if any.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// Code returns the code of err, or Internal for foreign errors.
func Code(err error) string {
	if e, ok := As(err); ok {
		return e.Code
	}
	return Internal
}
