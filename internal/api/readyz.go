package api

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// handleReadyz reports whether this process is ready to receive traffic.
// Unlike /healthz (liveness), readiness fails when required dependencies are unavailable.
func (s *Server) handleReadyz(c *gin.Context) {
	if s != nil && s.draining.Load() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":          "not_ready",
			"reason":          "draining",
			"in_flight_count": s.inFlightRequests.Load(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if s.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "config_not_loaded"})
		return
	}

	if dsn := strings.TrimSpace(s.cfg.Postgres.DSN); dsn != "" {
		if err := pingPostgres(ctx, dsn); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "postgres"})
			return
		}
	}

	if s.cfg.Redis.Enable {
		addr := strings.TrimSpace(s.cfg.Redis.Addr)
		if addr == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "redis_addr_missing"})
			return
		}
		if err := pingRedis(ctx, addr, s.cfg.Redis.Password, s.cfg.Redis.DB); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "redis"})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

func pingPostgres(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Second)
	return db.PingContext(ctx)
}

func pingRedis(ctx context.Context, addr, password string, db int) error {
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})
	defer client.Close()
	return client.Ping(ctx).Err()
}
