package usage

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// Request log rows of a platform account shared with a tenant.
//
// A tenant's request may run on a platform account shared with it. Its row is
// filed under the tenant and, as written by the executor, names the account by
// its channel name and source (the login email for OAuth accounts), with the
// credential id, label and email again inside the details: the diagnostic, the
// route trace and the upstream request log. The row is masked here, once,
// before it is stored, so every view that reads it (the log list and details,
// the monitor, the dashboards, exports) shows the account's stand-ins instead
// (coreauth.Auth.SharedName / SharedID). The platform still identifies the
// account by auth_index, which the row keeps.

// SharedAccountLister lists the platform accounts shared with a tenant.
type SharedAccountLister interface {
	ListSharedWithTenant(tenantID string) []*coreauth.Auth
}

// sharedAccountMaskTTL bounds how long a tenant's shared accounts are reused
// between log writes. A share granted or revoked meanwhile is picked up on the
// next refresh; listing the accounts on every write would clone them each time.
const sharedAccountMaskTTL = 30 * time.Second

type sharedAccountMask struct {
	index string
	name  string
	// replacer swaps the account's identities for its stand-ins in JSON text:
	// both sides are JSON-escaped, so the document stays valid and an identity
	// is found however the encoder wrote it.
	replacer *strings.Replacer
}

type sharedAccountMaskCache struct {
	lister SharedAccountLister
	mu     sync.Mutex
	byTen  map[string]sharedAccountMaskEntry
}

type sharedAccountMaskEntry struct {
	masks   []sharedAccountMask
	expires time.Time
}

var sharedAccountMasks struct {
	mu    sync.RWMutex
	cache *sharedAccountMaskCache
}

// InstallSharedAccountMasking makes request log writes mask the platform
// accounts lister reports as shared with the row's tenant. Without it rows are
// stored as written.
func InstallSharedAccountMasking(lister SharedAccountLister) {
	sharedAccountMasks.mu.Lock()
	defer sharedAccountMasks.mu.Unlock()
	if lister == nil {
		sharedAccountMasks.cache = nil
		return
	}
	sharedAccountMasks.cache = &sharedAccountMaskCache{lister: lister, byTen: make(map[string]sharedAccountMaskEntry)}
}

func (c *sharedAccountMaskCache) masksFor(tenantID string, now time.Time) []sharedAccountMask {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.byTen[tenantID]; ok && now.Before(entry.expires) {
		return entry.masks
	}
	var masks []sharedAccountMask
	for _, auth := range c.lister.ListSharedWithTenant(tenantID) {
		if auth == nil || !auth.SharedWith(tenantID) {
			continue
		}
		pairs := auth.SharedStandIns()
		args := make([]string, 0, 2*len(pairs))
		for _, pair := range pairs {
			args = append(args, jsonEscaped(pair[0]), jsonEscaped(pair[1]))
		}
		mask := sharedAccountMask{index: auth.EnsureIndex(), name: auth.SharedName()}
		if len(args) > 0 {
			mask.replacer = strings.NewReplacer(args...)
		}
		masks = append(masks, mask)
	}
	c.byTen[tenantID] = sharedAccountMaskEntry{masks: masks, expires: now.Add(sharedAccountMaskTTL)}
	return masks
}

// maskSharedAccountRow rewrites entry, a row filed under tenantID, so that no
// platform account shared with the tenant is named by its identity.
func maskSharedAccountRow(entry *RequestLogEntry, tenantID string) {
	if entry == nil {
		return
	}
	for _, mask := range sharedAccountMasksFor(tenantID) {
		if entry.AuthIndex != "" && entry.AuthIndex == mask.index {
			entry.Source = mask.name
			entry.ChannelName = mask.name
		}
		if mask.replacer == nil {
			continue
		}
		// The details name every account the request was routed past, not only
		// the one this row ran on.
		entry.DetailContent = mask.replacer.Replace(entry.DetailContent)
		// Upstream error bodies sometimes quote the account; successful replies
		// are the model's output and are left alone.
		if entry.Failed {
			entry.OutputContent = mask.replacer.Replace(entry.OutputContent)
		}
	}
}

// sharedAccountMasksFor returns the masks of tenantID's shared accounts, or
// nil for the platform or when masking is not installed.
func sharedAccountMasksFor(tenantID string) []sharedAccountMask {
	if isSystemTenant(tenantID) {
		return nil
	}
	sharedAccountMasks.mu.RLock()
	cache := sharedAccountMasks.cache
	sharedAccountMasks.mu.RUnlock()
	if cache == nil {
		return nil
	}
	return cache.masksFor(normalizeTenantID(tenantID), time.Now())
}

// MaskSharedAccountText rewrites stored log content read back for tenantID so
// that it does not name the platform accounts currently shared with it. Rows
// written since masking was installed are masked already (maskSharedAccountRow);
// this covers the ones stored before.
func MaskSharedAccountText(tenantID, text string) string {
	if text == "" {
		return text
	}
	for _, mask := range sharedAccountMasksFor(tenantID) {
		if mask.replacer != nil {
			text = mask.replacer.Replace(text)
		}
	}
	return text
}

func jsonEscaped(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) < 2 {
		return value
	}
	return string(encoded[1 : len(encoded)-1])
}
