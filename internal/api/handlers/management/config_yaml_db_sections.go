package management

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/identity"
	settingsstore "github.com/router-for-me/CLIProxyAPI/v8/internal/management/settings/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// rejectDatabaseOwnedSectionChanges answers a config.yaml save that changes a
// section the database owns besides the runtime settings (routing, proxy-pool,
// the API key sections), and reports whether it did. Once stored, the reload
// after the save would apply the stored value and drop such a section from
// config.yaml, so the save used to answer 200 while the change existed
// nowhere; API keys even served as keys of this node alone until the next
// start. The check runs before anything is stored or written, so a rejected
// save leaves the database, config.yaml and the live config as they were.
func (h *Handler) rejectDatabaseOwnedSectionChanges(c *gin.Context, cfg *config.Config, body []byte) bool {
	changed, err := settingsstore.DatabaseOwnedSectionsChangedByYAML(c.Request.Context(), cfg, body, h.systemRoutingCanonicalizer())
	if err != nil {
		log.WithError(err).Warn("management: config.yaml save rejected: reading the stored sections it spells out failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config.yaml was not saved: failed to read the stored sections it spells out: " + err.Error()})
		return true
	}
	if len(changed) == 0 {
		return false
	}
	keys := make([]string, 0, len(changed))
	for _, section := range changed {
		keys = append(keys, section.Key)
	}
	log.WithField("sections", keys).Info("management: config.yaml save rejected: it changes sections the database owns")
	// The structured body is what the panel reads the message from; a plain
	// string under "error" would be shown in place of it.
	c.JSON(http.StatusUnprocessableEntity, gin.H{"error": gin.H{
		"code":    "config_yaml_database_owned_sections",
		"message": databaseOwnedSectionsMessage(changed),
		"details": gin.H{"sections": keys},
	}})
	return true
}

// databaseOwnedSectionsMessage tells the operator which sections to take out
// of config.yaml and where each of them is edited instead.
func databaseOwnedSectionsMessage(changed []usage.DatabaseOwnedSection) string {
	keys := make([]string, 0, len(changed))
	var homes []string
	keysByHome := make(map[string][]string)
	for _, section := range changed {
		keys = append(keys, section.Key)
		if _, seen := keysByHome[section.Home]; !seen {
			homes = append(homes, section.Home)
		}
		keysByHome[section.Home] = append(keysByHome[section.Home], section.Key)
	}
	edits := make([]string, 0, len(homes))
	for _, home := range homes {
		edits = append(edits, joinWords(keysByHome[home])+" on "+home)
	}
	pronoun, noun := "them", "sections"
	if len(keys) == 1 {
		pronoun, noun = "it", "section"
	}
	return fmt.Sprintf("config.yaml was not saved: the database already holds %s, and owns %s, so the %s written here would be discarded. Remove %s from config.yaml and edit %s.",
		joinWords(keys), pronoun, noun, pronoun, strings.Join(edits, "; "))
}

// joinWords joins words as an English list: "a", "a and b", "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// systemRoutingCanonicalizer returns what PUT /routing-config applies to the
// system tenant's routing before storing it, the channel names resolved to
// their canonical spelling, so a copy of the stored routing that spells a
// channel differently still compares equal. When the known channels conflict
// that endpoint leaves the names as written, and so does this. The channels
// are collected on first use only: most saves have no routing to compare.
func (h *Handler) systemRoutingCanonicalizer() func(config.RoutingConfig) config.RoutingConfig {
	var (
		once  sync.Once
		known map[string]knownChannel
		err   error
	)
	return func(routing config.RoutingConfig) config.RoutingConfig {
		once.Do(func() {
			var auths []*coreauth.Auth
			if h.authManager != nil {
				auths = h.authManager.ListForTenant(identity.SystemTenantID)
			}
			h.mu.Lock()
			cfg := h.cfg
			h.mu.Unlock()
			known, err = collectKnownChannels(cfg, auths, "")
		})
		if err != nil {
			return routing
		}
		return canonicalizeRoutingConfigChannels(routing, known)
	}
}
