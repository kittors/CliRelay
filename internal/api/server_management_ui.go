package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/api/routes"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/managementasset"
	log "github.com/sirupsen/logrus"
)

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	routes.RegisterManagement(s.engine, s.mgmt, routes.ManagementOptions{
		Availability:       s.managementAvailabilityMiddleware(),
		PublicNoStore:      publicLookupNoStoreMiddleware(),
		PublicRateLimit:    s.publicLookupRateLimitMiddleware(),
		ClearWriteDeadline: clearServerWriteDeadline,
	})
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.managementRoutesEnabled.Load() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// setPanelSecurityHeaders hardens management panel HTML responses.
// frame-ancestors 'self' blocks cross-site clickjacking while still allowing
// same-origin embed menus configured inside the panel itself.
func setPanelSecurityHeaders(c *gin.Context) {
	c.Header("X-Frame-Options", "SAMEORIGIN")
	c.Header("Content-Security-Policy", "frame-ancestors 'self'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
	if cfg == nil || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	panelDir := s.resolvePanelDir()
	if panelDir == "" {
		filePath := managementasset.FilePath(s.configFilePath)
		if strings.TrimSpace(filePath) == "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if _, err := os.Stat(filePath); err != nil {
			if os.IsNotExist(err) {
				reqCtx := context.Background()
				if c != nil && c.Request != nil {
					if requestCtx := c.Request.Context(); requestCtx != nil {
						reqCtx = requestCtx
					}
				}
				if !managementasset.EnsureLatestManagementHTML(reqCtx, managementasset.StaticDir(s.configFilePath), cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository) {
					c.AbortWithStatus(http.StatusNotFound)
					return
				}
			} else {
				log.WithError(err).Error("failed to stat management control panel asset")
				c.AbortWithStatus(http.StatusInternalServerError)
				return
			}
		}
		setPanelSecurityHeaders(c)
		c.File(filePath)
		return
	}

	reqPath := strings.TrimSpace(c.Param("filepath"))
	if reqPath != "" && reqPath != "/" {
		cleanPath := filepath.Clean(strings.TrimPrefix(reqPath, "/"))
		if cleanPath != "." && !strings.Contains(cleanPath, "..") {
			candidate := filepath.Join(panelDir, cleanPath)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				s.serveStaticFileWithCompression(c, candidate)
				return
			}
		}
	}

	htmlFile := filepath.Join(panelDir, "manage.html")
	if _, err := os.Stat(htmlFile); err != nil {
		htmlFile = filepath.Join(panelDir, "management.html")
		if _, err = os.Stat(htmlFile); err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
	}
	setPanelSecurityHeaders(c)
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.File(htmlFile)
}

func clearServerWriteDeadline(c *gin.Context) {
	if c == nil || c.Writer == nil {
		return
	}
	_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
}

func (s *Server) resolvePanelDir() string {
	return managementasset.ResolvePanelDir(s.configFilePath)
}

func (s *Server) serveStaticFileWithCompression(c *gin.Context, filePath string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	base := filepath.Base(filePath)
	nameWithoutExt := strings.TrimSuffix(base, ext)
	if idx := strings.LastIndex(nameWithoutExt, "-"); idx > 0 && len(nameWithoutExt)-idx > 6 {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}

	compressible := map[string]bool{
		".js": true, ".css": true, ".svg": true, ".json": true,
		".html": true, ".xml": true, ".txt": true, ".map": true,
	}
	if !compressible[ext] {
		c.File(filePath)
		return
	}
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.File(filePath)
		return
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		c.File(filePath)
		return
	}

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		c.File(filePath)
		return
	}
	if _, err = gz.Write(data); err != nil {
		_ = gz.Close()
		c.File(filePath)
		return
	}
	if err = gz.Close(); err != nil {
		c.File(filePath)
		return
	}
	if buf.Len() >= len(data) {
		c.File(filePath)
		return
	}

	contentTypes := map[string]string{
		".js":   "application/javascript; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".svg":  "image/svg+xml",
		".json": "application/json; charset=utf-8",
		".html": "text/html; charset=utf-8",
		".xml":  "application/xml; charset=utf-8",
		".txt":  "text/plain; charset=utf-8",
		".map":  "application/json; charset=utf-8",
	}
	ct := contentTypes[ext]
	if ct == "" {
		ct = "application/octet-stream"
	}

	c.Header("Content-Encoding", "gzip")
	c.Header("Vary", "Accept-Encoding")
	c.Data(http.StatusOK, ct, buf.Bytes())
}
