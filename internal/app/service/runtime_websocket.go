package serviceapp

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/wsrelay"
	log "github.com/sirupsen/logrus"
)

type WebsocketGateway interface {
	Path() string
	Handler() http.Handler
	Stop(ctx context.Context) error
	RelayValue() any
}

type websocketGatewayBridge struct {
	manager *wsrelay.Manager
}

func (g *websocketGatewayBridge) Path() string {
	if g == nil || g.manager == nil {
		return ""
	}
	return g.manager.Path()
}

func (g *websocketGatewayBridge) Handler() http.Handler {
	if g == nil || g.manager == nil {
		return nil
	}
	return g.manager.Handler()
}

func (g *websocketGatewayBridge) Stop(ctx context.Context) error {
	if g == nil || g.manager == nil {
		return nil
	}
	return g.manager.Stop(ctx)
}

func (g *websocketGatewayBridge) RelayValue() any {
	if g == nil {
		return nil
	}
	return g.manager
}

func NewDefaultWebsocketGateway(onConnected func(string), onDisconnected func(string, error)) WebsocketGateway {
	opts := wsrelay.Options{
		Path:           "/v1/ws",
		OnConnected:    onConnected,
		OnDisconnected: onDisconnected,
		LogDebugf:      log.Debugf,
		LogInfof:       log.Infof,
		LogWarnf:       log.Warnf,
	}
	return &websocketGatewayBridge{manager: wsrelay.NewManager(opts)}
}
