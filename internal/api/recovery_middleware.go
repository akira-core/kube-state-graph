package api

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// recoveryMiddleware converts a panic on the handler goroutine (handlers or
// inner middleware) into the standard 500 JSON envelope instead of a TCP
// connection reset. It is registered innermost of the global chain (after
// requestID + logging) so the deferred access-log / HTTP-metric bookkeeping
// in loggingMiddleware still observes the written 500 status. Panics on
// goroutines the request path spawns are out of reach by Go semantics — the
// build fan-out recovers those at source (pkg/build ReadTopology's fetch
// closures).
//
// The panic value and stack are logged server-side only; the response body
// carries a static message (no internal detail, consistent with
// mapBuildError's sanitised 500). A deliberate non-import of gin.Recovery():
// its logger bypasses slog and its output format is not ours.
func (s *Server) recoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// http.ErrAbortHandler is net/http's sanctioned "abort this
			// response silently" sentinel (httputil.ReverseProxy and streaming
			// handlers panic with it on client disconnect). Re-panic so
			// conn.serve applies its documented quiet teardown — recovering it
			// here would turn deliberate aborts into ERROR logs with stacks
			// and a 500 written to a dead connection. Compared by value, as
			// net/http's own conn.serve does.
			if err, ok := rec.(error); ok && err == http.ErrAbortHandler { //nolint:errorlint // sentinel identity, mirrors net/http
				panic(rec)
			}
			s.logger.ErrorContext(c.Request.Context(), "panic recovered",
				"panic", fmt.Sprint(rec),
				"method", c.Request.Method,
				"path", c.FullPath(),
				"request_id", c.GetString("request_id"),
				"stack", string(debug.Stack()),
			)
			// If the handler already streamed part of a response we cannot
			// emit a coherent JSON envelope; otherwise return the standard
			// error body so clients get a parseable 500.
			if !c.Writer.Written() {
				writeError(c, http.StatusInternalServerError, "internal", "internal error")
			}
			c.Abort()
		}()
		c.Next()
	}
}
