package executor

import internalusage "github.com/router-for-me/CLIProxyAPI/v6/internal/usage"

// sameModelIdentity reports whether a requested model name and the model that
// was actually sent upstream denote the same model.
//
// The rule itself lives in the usage package because the management layer
// applies it too when rendering a stored row; see usage.SameModelIdentity for
// why a pure routing-prefix difference is not a different model.
func sameModelIdentity(requested, upstream string) bool {
	return internalusage.SameModelIdentity(requested, upstream)
}
