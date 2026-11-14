package api

import (
	"github.com/gin-gonic/gin"

	"github.com/akira-core/kube-state-graph/pkg/build"
)

// admissionMiddleware bounds the /v1/* requests in flight to
// --max-inflight-builds. Acquisition never waits: a request finding every slot
// taken is answered 503 overloaded with Retry-After at once and issues no
// upstream query, because queueing is what the per-store upstream slot already
// does, and the point here is to shed load before a request holds memory.
//
// It is mounted LAST on the /v1 group, after apiKeyMiddleware (a 401 stays a
// 401) and spanEnrichMiddleware (the 503's span is still marked). The slot and
// the gauge are released in a defer, so a panicking handler — recovered by the
// outer recovery middleware — cannot leak a slot.
//
// A nil slots channel (--max-inflight-builds=0) admits everything.
func (s *Server) admissionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.slots == nil {
			c.Next()
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			s.metrics.AdmissionRejected.Inc()
			s.mapBuildError(c, build.NewError(build.ReasonOverloaded, build.OverloadedMessage, nil))
			c.Abort()
			return
		}
		s.metrics.InflightBuilds.Inc()
		defer func() {
			s.metrics.InflightBuilds.Dec()
			<-s.slots
		}()
		c.Next()
	}
}
