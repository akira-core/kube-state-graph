package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"

	"github.com/marz32one/kube-state-graph/pkg/build"
)

// statusClientClosedRequest is the non-standard 499 (nginx convention) returned
// when the client disconnects mid-request. It is a 4xx, so it neither marks the
// server span Error nor inflates the 5xx request counter — a client cancellation
// is not a server/upstream fault.
const statusClientClosedRequest = 499

type errorBody struct {
	APIVersion string     `json:"apiVersion"`
	Error      errorField `json:"error"`
}

type errorField struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// writeError emits the JSON error body and owns the response-side telemetry that
// every error path shares: it stashes the machine reason for spanEnrichMiddleware
// (so a 5xx server span carries the precise reason rather than the generic
// "internal" fallback) and records a single exception event for any 5xx — giving
// encode / clusters / readyz failures the same span fidelity as build errors. A
// 4xx leaves the server span Unset and event-free (OTel HTTP semconv: a
// client-classifiable condition is not a server error). Span *status* is set in
// exactly one place — spanEnrichMiddleware — so it is never double-written.
func writeError(c *gin.Context, status int, reason, message string) {
	c.Set("build_reason", reason)
	if status >= 500 {
		if span := trace.SpanFromContext(c.Request.Context()); span.IsRecording() {
			span.RecordError(errors.New(message))
		}
	}
	c.JSON(status, errorBody{
		APIVersion: APIVersion,
		Error: errorField{
			Reason:  reason,
			Message: message,
		},
	})
}

// mapBuildError translates a typed build error into a REST-conventional HTTP
// status (RFC 9110 §15.6.3 Bad Gateway, §15.6.5 Gateway Timeout). A client
// cancellation maps to 499 (no 5xx pollution); span status/error recording is
// handled uniformly by writeError + spanEnrichMiddleware.
//
// The `reason` strings and status codes are contracts. The timeout, upstream,
// and default branches return static / build-authored messages: the wrapped
// promql error embeds the internal VictoriaMetrics URL/host/IP (`Post
// "http://...": dial tcp ...` — and build.Error.Error() stringifies the cause
// chain whenever a cause is attached), which must never reach a response body
// — the same redaction handleReadyz applies. The full error is logged
// server-side so operators keep the detail. Only outside_retention stays
// verbatim: it is constructed with a nil cause and a URL-free diagnostic
// message.
func (s *Server) mapBuildError(c *gin.Context, err error) {
	switch build.AsReason(err) {
	case build.ReasonTimeout:
		s.logger.ErrorContext(c.Request.Context(), "upstream query timed out",
			"err", err, "request_id", c.GetString("request_id"))
		writeError(c, http.StatusGatewayTimeout, "timeout", timeoutMessage(err))
	case build.ReasonOutsideRetention:
		writeError(c, http.StatusBadRequest, "outside_retention", err.Error())
	case build.ReasonUpstream:
		s.logger.ErrorContext(c.Request.Context(), "upstream query failed",
			"err", err, "request_id", c.GetString("request_id"))
		writeError(c, http.StatusBadGateway, "upstream", "upstream query failed")
	case build.ReasonCanceled:
		writeError(c, statusClientClosedRequest, "canceled", "request canceled")
	default:
		s.logger.ErrorContext(c.Request.Context(), "graph build failed",
			"err", err, "request_id", c.GetString("request_id"))
		writeError(c, http.StatusInternalServerError, "internal", "internal error")
	}
}

// timeoutMessage returns the build-authored static Message of a timeout error
// ("build timeout", "cluster discovery timed out", …) — never the cause
// chain, whose url.Error text embeds the internal upstream URL.
func timeoutMessage(err error) string {
	var be *build.Error
	if errors.As(err, &be) && be.Message != "" {
		return be.Message
	}
	return "request timed out"
}
