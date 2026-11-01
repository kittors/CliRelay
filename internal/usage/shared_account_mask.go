package usage

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"

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

// Implemented by the manager to avoid cloning credential/model-state maps.
type sharedAccountIdentityLister interface {
	ListSharedIdentitiesForTenant(tenantID string) []coreauth.SharedAccountIdentity
}

func sharedIdentitiesForTenant(lister SharedAccountLister, tenantID string) []coreauth.SharedAccountIdentity {
	if live, ok := lister.(sharedAccountIdentityLister); ok {
		return live.ListSharedIdentitiesForTenant(tenantID)
	}
	var identities []coreauth.SharedAccountIdentity
	for _, a := range lister.ListSharedWithTenant(tenantID) {
		if a != nil && a.SharedWith(tenantID) {
			identities = append(identities, coreauth.SharedAccountIdentity{AuthID: a.ID, Index: a.EnsureIndex(), Name: a.SharedName(), StandIns: a.SharedStandIns()})
		}
	}
	return identities
}

type sharedAccountMask struct {
	index string
	name  string
	// replacer swaps the account's identities for its stand-ins in JSON text:
	// both sides are JSON-escaped, so the document stays valid and an identity
	// is found however the encoder wrote it.
	replacer *strings.Replacer
}

type sharedAccountMaskSource struct {
	lister   SharedAccountLister
	mu       sync.Mutex
	compiled map[string]compiledSharedAccountMask
}

const maxCompiledSharedAccountMasks = 4096

type compiledSharedAccountMask struct {
	pairs [][2]string
	mask  sharedAccountMask
}

var sharedAccountMasks struct {
	mu     sync.RWMutex
	source *sharedAccountMaskSource
}

// InstallSharedAccountMasking makes request log writes mask the platform
// accounts lister reports as shared with the row's tenant. Without it rows are
// stored as written.
func InstallSharedAccountMasking(lister SharedAccountLister) {
	sharedAccountMasks.mu.Lock()
	defer sharedAccountMasks.mu.Unlock()
	if lister == nil {
		sharedAccountMasks.source = nil
	} else {
		sharedAccountMasks.source = &sharedAccountMaskSource{lister: lister, compiled: make(map[string]compiledSharedAccountMask)}
	}
}

// Build from the current grants and identities on every operation. A TTL cache
// (including an empty result) can disclose a newly shared account's login for
// its entire TTL. Refreshes, registrations and identity edits also change the
// strings to hide, so invalidating only on grant writes is not sufficient.
func (s *sharedAccountMaskSource) masksFor(tenantID string) []sharedAccountMask {
	// Never cache the authorized account list. Reuse only compiled replacements
	// whose exact live identity still matches; registration, refresh and grant
	// updates therefore take effect on the next operation without invalidation hooks.
	identities := sharedIdentitiesForTenant(s.lister, tenantID)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bound retained identities after credential churn; a full cache only skips
	// the optimization for new IDs, never masking itself.
	masks := make([]sharedAccountMask, 0, len(identities))
	for _, identity := range identities {
		pairs := identity.StandIns
		index, name := identity.Index, identity.Name
		cached, known := s.compiled[identity.AuthID]
		if known && cached.mask.index == index && cached.mask.name == name && slices.Equal(cached.pairs, pairs) {
			masks = append(masks, cached.mask)
			continue
		}
		args := make([]string, 0, 2*len(pairs))
		for _, pair := range pairs {
			args = append(args, jsonEscaped(pair[0]), jsonEscaped(pair[1]))
		}
		mask := sharedAccountMask{index: index, name: name}
		if len(args) > 0 {
			mask.replacer = strings.NewReplacer(args...)
		}
		if known || len(s.compiled) < maxCompiledSharedAccountMasks {
			s.compiled[identity.AuthID] = compiledSharedAccountMask{pairs: pairs, mask: mask}
		}
		masks = append(masks, mask)
	}
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
	source := sharedAccountMasks.source
	sharedAccountMasks.mu.RUnlock()
	if source == nil {
		return nil
	}
	return source.masksFor(normalizeTenantID(tenantID))
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
