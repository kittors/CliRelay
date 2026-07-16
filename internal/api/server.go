// Package api provides the HTTP API server implementation for the CLI Proxy API.
// It includes the server lifecycle and shared runtime state.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v6/internal/api/handlers/management"
	ampmodule "github.com/router-for-me/CLIProxyAPI/v6/internal/api/modules/amp"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

const (
	// Main API requests can legitimately spend several minutes waiting on upstream model execution.
	mainAPIServerWriteTimeout = 10 * time.Minute
)

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	// handlers contains the API handlers for processing requests.
	handlers *handlers.BaseAPIHandler

	// cfg holds the current server configuration.
	cfg *config.Config

	// oldConfigYaml stores a YAML snapshot of the previous configuration for change detection.
	// This prevents issues when the config object is modified in place by Management API.
	oldConfigYaml []byte

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// requestLogger is the request logger instance for dynamic configuration updates.
	requestLogger logging.RequestLogger
	loggerToggle  func(bool)

	// configFilePath is the absolute path to the YAML config file for persistence.
	configFilePath string

	// currentPath is the absolute path to the current working directory.
	currentPath string

	// wsRoutes tracks registered websocket upgrade paths.
	wsRouteMu     sync.Mutex
	wsRoutes      map[string]struct{}
	wsAuthChanged func(bool, bool)
	wsAuthEnabled atomic.Bool

	// management handler
	mgmt *managementHandlers.Handler

	// ampModule is the Amp routing module for model mapping hot-reload
	ampModule *ampmodule.AmpModule

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled atomic.Bool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()
	keepAliveHeartbeat chan struct{}
	keepAliveStop      chan struct{}

	publicLookupRateMu          sync.Mutex
	publicLookupRate            map[string]publicLookupRateLimitEntry
	publicLookupRateLastCleanup time.Time

	proxyWarmMu        sync.Mutex
	proxyWarmManager   interface{ Stop() }
	proxyWarmSignature string

	draining         atomic.Bool
	inFlightRequests atomic.Int64
}

// Start begins listening for and serving HTTP or HTTPS requests.
// It blocks until the listener exits or startup fails.
func (s *Server) Start() error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}

	useTLS := s.cfg != nil && s.cfg.TLS.Enable
	if useTLS {
		cert := strings.TrimSpace(s.cfg.TLS.Cert)
		key := strings.TrimSpace(s.cfg.TLS.Key)
		if cert == "" || key == "" {
			return fmt.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
		}
		log.Debugf("Starting API server on %s with TLS", s.server.Addr)
		if errServeTLS := s.server.ListenAndServeTLS(cert, key); errServeTLS != nil && !errors.Is(errServeTLS, http.ErrServerClosed) {
			return fmt.Errorf("failed to start HTTPS server: %v", errServeTLS)
		}
		return nil
	}

	log.Debugf("Starting API server on %s", s.server.Addr)
	if errServe := s.server.ListenAndServe(); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
		return fmt.Errorf("failed to start HTTP server: %v", errServe)
	}

	return nil
}

// Stop gracefully shuts down the API server without interrupting any
// active connections and closes the management handler before the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	log.Debug("Stopping API server...")
	s.draining.Store(true)
	s.server.SetKeepAlivesEnabled(false)
	log.Infof("API server entering draining mode with %d in-flight request(s)", s.inFlightRequests.Load())

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	if s.mgmt != nil {
		s.mgmt.Close()
	}
	s.stopProxyWarmup()

	// Shutdown the HTTP server.
	if err := s.server.Shutdown(ctx); err != nil {
		log.Errorf("API server shutdown timed out with %d in-flight request(s)", s.inFlightRequests.Load())
		return fmt.Errorf("failed to shutdown HTTP server: %v", err)
	}

	log.Debug("API server stopped")
	return nil
}
