package cliproxy

import (
	"hash/fnv"
	"sync"
)

// registrationLocks serialises registration per credential.
//
// Registration reads a credential's last known model list and then registers
// what it derived from it. Several paths register concurrently — the startup
// pass, the credential update queue, model library refreshes, and the background
// fetch that just stored a newer list. Without the lock, a registration that read
// the older list could land after the one that read the newer list and put the
// older models back. Holding the lock across the read and the registry write
// makes the last registration the one working from the newest list.
//
// A fixed set of stripes needs no cleanup when credentials go away; two
// credentials sharing a stripe only wait on each other's registration, which no
// longer involves the network.
type registrationLocks struct {
	stripes [64]sync.Mutex
}

func (r *registrationLocks) lock(authID string) func() {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(authID))
	stripe := &r.stripes[hash.Sum32()%uint32(len(r.stripes))]
	stripe.Lock()
	return stripe.Unlock
}
