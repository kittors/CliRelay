package cliproxy

import (
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// logModelRegistration records what each credential ended up serving.
//
// Which branch a credential takes, and how many models survive the exclusion and
// alias filters, is otherwise invisible: the panel shows a merged view and the
// registry has no read-only surface. Diagnosing "this account cannot serve model
// X" then means guessing at the branch, which has repeatedly been the slow part of
// real incidents.
//
// One line per credential at startup and on credential change is cheap, and names
// the branch so the next investigation starts from fact rather than inference.
func logModelRegistration(auth *coreauth.Auth, provider, authKind, branch string, models []*ModelInfo) {
	if auth == nil {
		return
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil && strings.TrimSpace(model.ID) != "" {
			ids = append(ids, model.ID)
		}
	}
	log.Infof(
		"model registration: auth=%s provider=%q kind=%q branch=%s models=%d [%s]",
		auth.ID, provider, authKind, branch, len(ids), strings.Join(ids, " "),
	)
}
