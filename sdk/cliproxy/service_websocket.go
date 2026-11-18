package cliproxy

import (
	"context"
	"net/http"
	"time"

	serviceapp "github.com/router-for-me/CLIProxyAPI/v8/sdkbridge/service"
	log "github.com/sirupsen/logrus"
)

type websocketGateway interface {
	Path() string
	Handler() http.Handler
	Stop(ctx context.Context) error
	RelayValue() any
}

func newDefaultWebsocketGateway(s *Service) websocketGateway {
	if s == nil {
		return nil
	}
	return serviceapp.NewDefaultWebsocketGateway(s.wsOnConnected, s.wsOnDisconnected)
}

func (s *Service) bindWebsocketGateway(ctx context.Context) {
	if s == nil {
		return
	}
	s.ensureWebsocketGateway()
	if s.server == nil || s.wsGateway == nil {
		return
	}
	s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
	s.server.SetWebsocketAuthChangeHandler(func(oldEnabled, newEnabled bool) {
		if oldEnabled == newEnabled {
			return
		}
		if !oldEnabled && newEnabled {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if errStop := s.wsGateway.Stop(stopCtx); errStop != nil {
				log.Warnf("failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled, newEnabled, errStop)
				return
			}
			log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
			return
		}
		log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
	})
}
