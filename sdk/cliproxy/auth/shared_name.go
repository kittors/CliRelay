package auth

import (
	"path/filepath"
	"sort"
	"strings"
)

// How a platform account is named to a tenant it is shared with.
//
// A tenant reaches the platform's accounts only through sharing, and must not
// learn whose they are: the address an OAuth login was made with is the
// account's identity at the upstream, and the credential id and file name are
// usually built from it (claude-<email>.json). So wherever a tenant would see a
// shared account (its request logs, the route trace, the in-flight view, the
// routing simulator, its list of shared accounts) the account goes by a stand-in
// name and id instead. The platform's own views are unchanged.

// sharedIdentityMinLength keeps short, generic values (a provider name, a
// one-word label) from being treated as an identity to replace in free text.
const sharedIdentityMinLength = 6

// SharedName is the account's name for a tenant it is shared with: the label
// the platform gave it, unless that label is an address, and otherwise its
// provider and a short tag of its index (which the tenant sees anyway). It never
// returns the email, the file name or the credential id.
func (a *Auth) SharedName() string {
	if a == nil {
		return ""
	}
	if label := strings.TrimSpace(a.Label); label != "" && !looksLikeAddress(label) {
		return label
	}
	if a.Metadata != nil {
		if raw, ok := a.Metadata["label"].(string); ok {
			if label := strings.TrimSpace(raw); label != "" && !looksLikeAddress(label) {
				return label
			}
		}
	}
	provider := strings.TrimSpace(a.Provider)
	if provider == "" {
		provider = "account"
	}
	tag := a.currentIndex()
	if len(tag) > 8 {
		tag = tag[:8]
	}
	if tag == "" {
		return provider
	}
	return provider + " · " + tag
}

// SharedID stands in for the credential id where a tenant would see it. It is
// stable, so steps of one request that name the same account still match.
func (a *Auth) SharedID() string {
	if a == nil {
		return ""
	}
	return "shared-" + a.currentIndex()
}

// SharedStandIns lists the strings that identify the account at its upstream,
// each with what a tenant sees instead, longest first so that a value containing
// another (a file name containing the email) is replaced whole.
func (a *Auth) SharedStandIns() [][2]string {
	if a == nil {
		return nil
	}
	name, id := a.SharedName(), a.SharedID()
	seen := make(map[string]struct{})
	var pairs [][2]string
	add := func(value, standIn string) {
		value = strings.TrimSpace(value)
		// What the shared name itself shows is not hidden by definition: the
		// platform chose that label (one holding the file name, say).
		if len(value) < sharedIdentityMinLength || strings.Contains(name, value) || value == id {
			return
		}
		if _, dup := seen[value]; dup {
			return
		}
		seen[value] = struct{}{}
		pairs = append(pairs, [2]string{value, standIn})
	}
	add(a.ID, id)
	if file := strings.TrimSpace(a.FileName); file != "" {
		add(file, id)
		add(filepath.Base(file), id)
	}
	addAddress := func(value string) {
		if looksLikeAddress(value) {
			add(value, name)
		}
	}
	addAddress(a.Label)
	if a.Metadata != nil {
		for _, key := range []string{"email", "label", "account", "username"} {
			if raw, ok := a.Metadata[key].(string); ok {
				addAddress(raw)
			}
		}
	}
	if _, info := a.AccountInfo(); info != "" {
		addAddress(info)
	}
	sort.SliceStable(pairs, func(i, j int) bool { return len(pairs[i][0]) > len(pairs[j][0]) })
	return pairs
}

// SharedWith reports whether tenantID sees a as a shared account: one it can
// use but another tenant owns.
func (a *Auth) SharedWith(tenantID string) bool {
	return a != nil && normalizedTenantID(a.TenantID) != normalizedTenantID(tenantID)
}

func looksLikeAddress(value string) bool {
	return strings.Contains(value, "@")
}
