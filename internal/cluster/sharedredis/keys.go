package sharedredis

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// KeyPrefix namespaces every key CliRelay writes to the cluster Redis, so the
// instance can be shared with other tenants of the machine and inspected with
// a single SCAN pattern.
const KeyPrefix = "clirelay:"

// HashPart turns identifiers into a fixed-length key segment.
//
// Several identifiers must not appear in key names: API keys are secrets, and
// session keys can be derived from request content. The cluster Redis is
// reached over the public network and may be inspected by whoever operates
// that machine, so key names only ever carry this digest. 128 bits keep
// collisions out of reach while halving the key size.
func HashPart(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}
