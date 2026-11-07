package enduser

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota/inflight"
)

// keyUsageView is a key's usage view with what its requests in flight on this
// node hold. The admission checks count that as used, so a key refused below
// its limit shows why.
func keyUsageView(keyID string, snapshot quota.UsageSnapshot, rules []quota.ModelRule) quota.UsageView {
	view := snapshot.UsageView(rules)
	view.InFlight = inflight.Default().Held(inflight.KeyName(keyID)).Usage()
	return view
}
