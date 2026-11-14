package api

import (
	"net/http"
	"time"

	"github.com/akira-core/kube-state-graph/internal/config"
)

// MaxHeaderBytes bounds the request line plus headers of one request. It is set
// explicitly rather than left at net/http's 1 MiB default: the largest request
// every graph parser and root cap admits at the default byte budget is 236779
// bytes (~231 KiB) of request line (100 × 253-byte values for each of the four
// selector parameters plus 512 253-byte `aggr` roots filling the chunk cap,
// measured by TestMaxHeaderBytes_AdmitsTheLargestLegalRequest), so 256 KiB
// admits it with room for headers while refusing anything larger with 431
// before a handler runs.
const MaxHeaderBytes = 256 << 10

// NewHTTPServer returns the http.Server the binary runs: the handler, the
// listen address, the read / write timeouts derived from cfg, and
// MaxHeaderBytes. It is the one constructor cmd/kube-state-graph uses, so a test
// can drive exactly the production transport limits over a real listener.
func NewHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      cfg.BuildTimeout + 5*time.Second,
		MaxHeaderBytes:    MaxHeaderBytes,
	}
}
