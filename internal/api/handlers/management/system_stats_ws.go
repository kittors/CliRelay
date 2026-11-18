package management

import (
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	log "github.com/sirupsen/logrus"
)

var statsUpgrader = websocket.Upgrader{
	CheckOrigin: util.WebsocketOriginAllowed,
}

// SystemStatsWebSocket handles GET /v0/management/system-stats/ws.
// It pushes SystemStats JSON at a configurable interval and periodically rotates
// only this management connection so nginx reloads can retire old workers.
// Public model streaming endpoints are not affected.
func (h *Handler) SystemStatsWebSocket(c *gin.Context) {
	conn, err := statsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Warnf("system-stats ws: upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	interval := 3 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	maxAgeTimer := time.NewTimer(h.systemStatsWebSocketMaxAge())
	defer maxAgeTimer.Stop()

	// Background reader: listen for client messages to adjust interval
	clientMsg := make(chan json.RawMessage, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case clientMsg <- json.RawMessage(msg):
			default:
			}
		}
	}()

	// Send initial stats immediately
	if data, err := json.Marshal(h.collectSystemStats()); err == nil {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
	}

	for {
		select {
		case <-done:
			return
		case <-maxAgeTimer.C:
			deadline := time.Now().Add(2 * time.Second)
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "connection rotation"),
				deadline,
			)
			return
		case msg := <-clientMsg:
			// Parse interval change request
			var req struct {
				Interval int `json:"interval"`
			}
			if json.Unmarshal(msg, &req) == nil && req.Interval >= 1 && req.Interval <= 60 {
				ticker.Stop()
				interval = time.Duration(req.Interval) * time.Second
				ticker = time.NewTicker(interval)
				log.Debugf("system-stats ws: interval changed to %ds", req.Interval)
			}
		case <-ticker.C:
			stats := h.collectSystemStats()
			data, err := json.Marshal(stats)
			if err != nil {
				continue
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}
}

func (h *Handler) systemStatsWebSocketMaxAge() time.Duration {
	if h == nil {
		return config.DefaultSystemStatsWebSocketMaxAge
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.SystemStatsWebSocketMaxAge()
}
