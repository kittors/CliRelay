package management

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/ipaccess"
)

// maxBulkRuleIDs caps one bulk request. Operators select from a page, so this is
// far above any real selection while keeping a single request bounded.
const maxBulkRuleIDs = 200

// PatchIPAccessRulesBulk applies one change to many rules.
//
// It exists because the realistic cleanup after an attack is "disable these
// fourteen automatic bans", and doing that one request at a time is both slow
// and partially-applied when something fails halfway.
func (h *Handler) PatchIPAccessRulesBulk(c *gin.Context) {
	registry := ipaccess.Default()
	if registry == nil {
		ipAccessUnavailable(c)
		return
	}
	var body struct {
		IDs       []string `json:"ids"`
		Enabled   *bool    `json:"enabled"`
		Delete    bool     `json:"delete"`
		ExpiresAt *string  `json:"expires_at"`
		Note      *string  `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		ipAccessBadRequest(c, err)
		return
	}
	if len(body.IDs) == 0 {
		ipAccessBadRequest(c, errors.New("no rule ids supplied"))
		return
	}
	if len(body.IDs) > maxBulkRuleIDs {
		ipAccessBadRequest(c, errors.New("too many rule ids in one request"))
		return
	}

	input := ipaccess.UpdateInput{Enabled: body.Enabled, Note: body.Note}
	if body.ExpiresAt != nil {
		if strings.TrimSpace(*body.ExpiresAt) == "" {
			input.ClearExpires = true
		} else {
			expires, err := time.Parse(time.RFC3339, *body.ExpiresAt)
			if err != nil {
				ipAccessBadRequest(c, err)
				return
			}
			pointer := &expires
			input.ExpiresAt = &pointer
		}
	}

	// Per-id outcomes rather than one all-or-nothing status: a rule another
	// operator just deleted should not fail the other thirteen, and the caller
	// still needs to know which ones did not apply.
	applied := make([]string, 0, len(body.IDs))
	failed := make(map[string]string)
	for _, id := range body.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		var err error
		if body.Delete {
			err = registry.Store().Delete(c.Request.Context(), id)
		} else {
			_, err = registry.Store().Update(c.Request.Context(), id, input)
		}
		if err != nil {
			failed[id] = err.Error()
			continue
		}
		applied = append(applied, id)
	}
	if len(applied) > 0 {
		_ = registry.Refresh(c.Request.Context())
	}
	c.JSON(http.StatusOK, gin.H{"applied": applied, "failed": failed})
}
