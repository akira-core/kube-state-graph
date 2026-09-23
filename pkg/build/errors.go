package build

import (
	"errors"

	"github.com/akira-core/kube-state-graph/pkg/promql"
)

// Reason classifies a build failure for HTTP status mapping.
type Reason string

const (
	ReasonTimeout          Reason = "timeout"
	ReasonOutsideRetention Reason = "outside_retention"
	ReasonUpstream         Reason = "upstream"
	// ReasonCanceled marks a build aborted by client disconnect
	// (context.Canceled). It is a client action, not a server/upstream fault,
	// so the API layer maps it to a 4xx and avoids 5xx metric / span-error
	// pollution.
	ReasonCanceled Reason = "canceled"
)

// Error wraps an underlying cause with a typed Reason for HTTP mapping.
//
// Query names the upstream query family whose error failed the build, when
// one did (fail-storage-graph-on-any-leg-error D2). It is always a bare
// promql.Query constant — never text taken from the upstream error, which can
// embed an internal URL — so the API layer may put it in a response body.
type Error struct {
	Reason  Reason
	Message string
	Query   string
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Reason) + ": " + e.Err.Error()
	}
	return string(e.Reason) + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// AsReason returns the typed Reason of err, or "" if it is not a build.Error.
func AsReason(err error) Reason {
	if be, ok := errors.AsType[*Error](err); ok {
		return be.Reason
	}
	return ""
}

// QueryError attaches the bare family name to the error one upstream query
// returned, so the build error that finally reaches the API layer can say
// WHICH family failed without echoing the upstream error text. It wraps, and
// errors.Is sees through it, so cancellation and deadline classification are
// unchanged.
type QueryError struct {
	Query string
	Err   error
}

func (e *QueryError) Error() string { return e.Query + " query: " + e.Err.Error() }

func (e *QueryError) Unwrap() error { return e.Err }

// wrapQueryError names err with its family. A nil err stays nil, so a call
// site can wrap its return value unconditionally.
func wrapQueryError(name promql.Query, err error) error {
	if err == nil {
		return nil
	}
	return &QueryError{Query: string(name), Err: err}
}

// NewError constructs a build.Error.
func NewError(reason Reason, message string, cause error) error {
	return &Error{Reason: reason, Message: message, Err: cause}
}
