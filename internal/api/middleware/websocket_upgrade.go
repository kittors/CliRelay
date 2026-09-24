package middleware

import (
	"net/http"

	"github.com/gorilla/websocket"
)

// IsWebsocketUpgrade reports whether r is a WebSocket upgrade, using the same
// check the gorilla upgrader applies before it accepts one. The middleware that
// acts on upgraded connections (quota, model restriction) shares this single
// check, so every upgrade the handler accepts is gated.
func IsWebsocketUpgrade(r *http.Request) bool {
	return r != nil && r.Method == http.MethodGet && websocket.IsWebSocketUpgrade(r)
}
