// Package serviceapp hosts internal service/runtime assembly bridges.
//
// sdkbridge/service imports this package to expose a narrow set of watcher,
// executor-binding, websocket, and runtime-setting helpers without leaking the
// wider internal runtime graph into sdk packages.
package serviceapp

import (
	"context"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type RuntimeAuthUpdateAction string

const (
	RuntimeAuthUpdateActionAdd    RuntimeAuthUpdateAction = "add"
	RuntimeAuthUpdateActionModify RuntimeAuthUpdateAction = "modify"
	RuntimeAuthUpdateActionDelete RuntimeAuthUpdateAction = "delete"
)

type RuntimeAuthUpdate struct {
	Action RuntimeAuthUpdateAction
	ID     string
	Auth   *coreauth.Auth
}

type WatcherBridge interface {
	Start(ctx context.Context) error
	Stop() error
	SetConfig(cfg *config.Config)
	SnapshotCoreAuths() []*coreauth.Auth
	SetAuthUpdateQueue(queue chan<- RuntimeAuthUpdate)
	DispatchRuntimeAuthUpdate(update RuntimeAuthUpdate) bool
}

type watcherBridge struct {
	w *watcher.Watcher

	queueMu     sync.Mutex
	queueCancel context.CancelFunc
}

func NewWatcher(configPath, authDir string, reload func(*config.Config)) (WatcherBridge, error) {
	w, err := watcher.NewWatcher(configPath, authDir, reload)
	if err != nil {
		return nil, err
	}
	return &watcherBridge{w: w}, nil
}

func (b *watcherBridge) Start(ctx context.Context) error {
	if b == nil || b.w == nil {
		return nil
	}
	return b.w.Start(ctx)
}

func (b *watcherBridge) Stop() error {
	if b == nil || b.w == nil {
		return nil
	}
	b.setQueue(nil)
	return b.w.Stop()
}

func (b *watcherBridge) SetConfig(cfg *config.Config) {
	if b == nil || b.w == nil {
		return
	}
	b.w.SetConfig(cfg)
}

func (b *watcherBridge) SnapshotCoreAuths() []*coreauth.Auth {
	if b == nil || b.w == nil {
		return nil
	}
	return b.w.SnapshotCoreAuths()
}

func (b *watcherBridge) SetAuthUpdateQueue(queue chan<- RuntimeAuthUpdate) {
	if b == nil || b.w == nil {
		return
	}
	b.setQueue(queue)
}

func (b *watcherBridge) DispatchRuntimeAuthUpdate(update RuntimeAuthUpdate) bool {
	if b == nil || b.w == nil {
		return false
	}
	return b.w.DispatchRuntimeAuthUpdate(toWatcherAuthUpdate(update))
}

func (b *watcherBridge) setQueue(queue chan<- RuntimeAuthUpdate) {
	b.queueMu.Lock()
	defer b.queueMu.Unlock()

	if b.queueCancel != nil {
		b.queueCancel()
		b.queueCancel = nil
	}
	if queue == nil {
		b.w.SetAuthUpdateQueue(nil)
		return
	}

	internalQueue := make(chan watcher.AuthUpdate, 256)
	forwardCtx, cancel := context.WithCancel(context.Background())
	b.queueCancel = cancel
	b.w.SetAuthUpdateQueue(internalQueue)

	go func() {
		for {
			select {
			case <-forwardCtx.Done():
				return
			case update, ok := <-internalQueue:
				if !ok {
					return
				}
				select {
				case queue <- fromWatcherAuthUpdate(update):
				case <-forwardCtx.Done():
					return
				}
			}
		}
	}()
}

func toWatcherAuthUpdate(update RuntimeAuthUpdate) watcher.AuthUpdate {
	return watcher.AuthUpdate{
		Action: watcher.AuthUpdateAction(update.Action),
		ID:     update.ID,
		Auth:   update.Auth,
	}
}

func fromWatcherAuthUpdate(update watcher.AuthUpdate) RuntimeAuthUpdate {
	return RuntimeAuthUpdate{
		Action: RuntimeAuthUpdateAction(update.Action),
		ID:     update.ID,
		Auth:   update.Auth,
	}
}
