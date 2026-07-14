package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	ksgdocs "github.com/akira-core/kube-state-graph/docs"
)

// scalarHTML is the Scalar API Reference UI, loaded from the official CDN.
// This is Scalar's documented minimal integration: load the standalone bundle,
// then call Scalar.createApiReference pointed at our same-origin OpenAPI spec
// (so no proxyUrl is needed). See https://github.com/scalar/scalar.
//
// The bundle is pinned to an exact version (not "latest") and carries a
// Subresource Integrity hash so a compromised or mutated CDN artifact cannot
// silently execute — jsDelivr serves versioned files immutably, so the hash is
// stable. handleDocs adds CSP / framing / sniffing response headers.
const scalarHTML = `<!doctype html>
<html lang="en">
    <head>
        <meta charset="utf-8" />
        <meta name="viewport" content="width=device-width, initial-scale=1" />
        <title>kube-state-graph API reference</title>
    </head>
    <body>
        <div id="app"></div>
        <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.58.0/dist/browser/standalone.min.js"
                integrity="sha384-OYnMCMKIwY/7+TI/gqX7yuLg86XGVgTUd04CL3OTv3nKKKxY1fmsMPMIu+7NtVcq"
                crossorigin="anonymous"></script>
        <script>
            Scalar.createApiReference('#app', { url: '/openapi.json' })
        </script>
    </body>
</html>
`

// handleOpenAPIYAML serves the embedded OpenAPI 3.1 spec in YAML form.
//
//	@Summary	OpenAPI spec (YAML)
//	@Tags		docs
//	@Produce	application/yaml
//	@Success	200	{string}	string	"OpenAPI 3.1 YAML"
//	@Router		/openapi.yaml [get]
func (s *Server) handleOpenAPIYAML(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", ksgdocs.OpenAPIYAML)
}

// handleOpenAPIJSON serves the embedded OpenAPI 3.1 spec in JSON form.
//
//	@Summary	OpenAPI spec (JSON)
//	@Tags		docs
//	@Produce	application/json
//	@Success	200	{string}	string	"OpenAPI 3.1 JSON"
//	@Router		/openapi.json [get]
func (s *Server) handleOpenAPIJSON(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/json; charset=utf-8", ksgdocs.OpenAPIJSON)
}

// handleDocs renders the Scalar API Reference UI.
//
//	@Summary	API reference UI (Scalar)
//	@Tags		docs
//	@Produce	text/html
//	@Success	200	{string}	string	"HTML"
//	@Router		/docs [get]
func (s *Server) handleDocs(c *gin.Context) {
	c.Header("Cache-Control", "public, max-age=300")
	// Restrict the docs page to the Scalar CDN + same-origin spec. The inline
	// Scalar.createApiReference call forces script-src 'unsafe-inline'; the rest
	// is scoped to jsDelivr and 'self'. Plus clickjacking / MIME-sniffing guards.
	c.Header("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; "+
			"style-src 'unsafe-inline' https://cdn.jsdelivr.net; "+
			"img-src 'self' data: https:; "+
			"font-src https://cdn.jsdelivr.net data:; "+
			"connect-src 'self' https://cdn.jsdelivr.net; "+
			"worker-src 'self' blob:")
	c.Header("X-Frame-Options", "DENY")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(scalarHTML))
}
