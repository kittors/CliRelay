package authfiles

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// BindProxyID attaches a proxy-pool entry to an auth record.
//
// Both places have to be written. Auth.ProxyID is what the runtime reads when it
// picks an egress route — APICallTransport and the executors resolve it through
// Config.ResolveProxyURL — while metadata["proxy_id"] is what survives to disk
// and is read back by RecordFromMetadata on reload. Setting only the field gives
// a binding that disappears on restart; setting only the metadata gives one that
// does not take effect until then.
//
// An empty proxyID is a no-op rather than an unbind: callers that mean to clear a
// binding go through the patch path, which distinguishes "not supplied" from
// "set to empty".
func BindProxyID(auth *coreauth.Auth, proxyID string) {
	if auth == nil {
		return
	}
	proxyID = strings.TrimSpace(proxyID)
	if proxyID == "" {
		return
	}
	auth.ProxyID = proxyID
	ensureMetadata(auth)["proxy_id"] = proxyID
}
