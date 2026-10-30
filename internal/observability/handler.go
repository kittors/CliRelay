package observability

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// scrapeTimeout bounds one gather; a scrape that has not finished by
	// then is answered 503 rather than left holding a connection.
	scrapeTimeout = 30 * time.Second
	// maxConcurrentScrapes refuses pile-ups from a misconfigured scraper.
	maxConcurrentScrapes = 4
)

// Handler serves the exposition. It answers 404 while the endpoint is off, so
// an unconfigured deployment does not even reveal that it has one, and 401
// without the right bearer token.
func (e *Exporter) Handler() gin.HandlerFunc {
	inner := promhttp.HandlerFor(e.registry, promhttp.HandlerOpts{
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: maxConcurrentScrapes,
		Timeout:             scrapeTimeout,
	})
	return func(c *gin.Context) {
		settings := e.Settings()
		if !settings.Enabled || !settings.HasToken() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if !tokenMatches(bearerToken(c.GetHeader("Authorization")), settings.TokenSHA256) {
			c.Header("WWW-Authenticate", `Bearer realm="clirelay-metrics"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Header("Cache-Control", "no-store")
		inner.ServeHTTP(c.Writer, c.Request)
	}
}

// bearerToken extracts the credential of an "Authorization: Bearer" header.
// The token is accepted nowhere else: a query parameter would end up in access
// logs.
func bearerToken(header string) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// HTTPMiddleware counts every request the router serves. Install it outside
// the panic recovery middleware so a recovered panic is counted with the 500
// it was answered with.
func (e *Exporter) HTTPMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		method := ""
		if c.Request != nil {
			method = c.Request.Method
		}
		e.ObserveHTTP(method, c.FullPath(), c.Writer.Status(), time.Since(start))
	}
}
