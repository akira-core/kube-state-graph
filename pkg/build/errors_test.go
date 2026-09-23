package build

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestClassifyReadError_MapsByCause guards F15: a client cancellation
// (context.Canceled) classifies as ReasonCanceled (→ 4xx, no 5xx/span-error
// pollution), distinct from a build timeout (DeadlineExceeded → ReasonTimeout)
// and a generic upstream failure (→ ReasonUpstream).
func TestClassifyReadError_MapsByCause(t *testing.T) {
	t.Parallel()
	span := trace.SpanFromContext(t.Context()) // non-recording no-op span
	cases := []struct {
		name string
		err  error
		want Reason
	}{
		{"canceled", fmt.Errorf("fan-out: %w", context.Canceled), ReasonCanceled},
		{"deadline", fmt.Errorf("fan-out: %w", context.DeadlineExceeded), ReasonTimeout},
		{"upstream", errors.New("dial tcp: connection refused"), ReasonUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyReadError(span, "topology read failed", tc.err)
			if AsReason(got) != tc.want {
				t.Errorf("classifyReadError reason=%q want %q", AsReason(got), tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("classifyReadError must preserve the cause via Unwrap")
			}
		})
	}
}

func TestNewError_FieldsRoundTrip(t *testing.T) {
	t.Parallel()
	cause := errors.New("dns")
	e := NewError(ReasonUpstream, "msg", cause)
	var be *Error
	if !errors.As(e, &be) {
		t.Fatalf("errors.As did not match *Error")
	}
	if be.Reason != ReasonUpstream {
		t.Errorf("reason=%q", be.Reason)
	}
	if be.Message != "msg" {
		t.Errorf("message=%q", be.Message)
	}
	if !errors.Is(e, cause) {
		t.Errorf("errors.Is did not unwrap to cause")
	}
}

func TestError_StringFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{"with cause", &Error{Reason: ReasonTimeout, Message: "ignored", Err: errors.New("ctx")}, "timeout: ctx"},
		{"without cause", &Error{Reason: ReasonOutsideRetention, Message: "no rows"}, "outside_retention: no rows"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestAsReason_ExtractsThroughWrap(t *testing.T) {
	t.Parallel()
	inner := NewError(ReasonTimeout, "deadline", nil)
	wrapped := fmt.Errorf("handler: %w", inner)
	if got := AsReason(wrapped); got != ReasonTimeout {
		t.Errorf("AsReason wrapped=%q want timeout", got)
	}
}

func TestAsReason_NonBuildError(t *testing.T) {
	t.Parallel()
	if got := AsReason(errors.New("plain")); got != "" {
		t.Errorf("AsReason plain=%q want empty", got)
	}
	if got := AsReason(nil); got != "" {
		t.Errorf("AsReason nil=%q want empty", got)
	}
}

func TestError_Unwrap_NilCause(t *testing.T) {
	t.Parallel()
	e := &Error{Reason: ReasonOutsideRetention, Message: "x"}
	if e.Unwrap() != nil {
		t.Errorf("Unwrap nil cause should return nil")
	}
}

// A query error wrapped with its family keeps its classification and carries
// the family onto the build error; an unwrapped error carries none.
func TestClassifyReadError_CarriesQueryFamily(t *testing.T) {
	t.Parallel()
	span := trace.SpanFromContext(t.Context())
	cases := []struct {
		name      string
		err       error
		wantR     Reason
		wantQuery string
	}{
		{"named upstream", fmt.Errorf("fan-out: %w", wrapQueryError("volume_labels", errors.New("503"))), ReasonUpstream, "volume_labels"},
		{"named deadline", wrapQueryError("volume_labels", context.DeadlineExceeded), ReasonTimeout, ""},
		{"named cancel", wrapQueryError("volume_labels", context.Canceled), ReasonCanceled, ""},
		{"unnamed upstream", errors.New("503"), ReasonUpstream, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyReadError(span, "topology read failed", tc.err)
			be, ok := errors.AsType[*Error](got)
			if !ok {
				t.Fatalf("not a build.Error: %v", got)
			}
			if be.Reason != tc.wantR || be.Query != tc.wantQuery {
				t.Errorf("got (%q, %q) want (%q, %q)", be.Reason, be.Query, tc.wantR, tc.wantQuery)
			}
		})
	}
	if wrapQueryError("x", nil) != nil {
		t.Error("wrapping a nil error must stay nil")
	}
}
