package cliproxy

import "github.com/router-for-me/CLIProxyAPI/v8/internal/watcher"

// runtimeAuthUpdate is retained as a source-compatible alias for older embedders.
type runtimeAuthUpdate = watcher.AuthUpdate
type runtimeAuthUpdateAction = watcher.AuthUpdateAction

const (
	runtimeAuthUpdateActionAdd    = watcher.AuthUpdateActionAdd
	runtimeAuthUpdateActionModify = watcher.AuthUpdateActionModify
	runtimeAuthUpdateActionDelete = watcher.AuthUpdateActionDelete
)
