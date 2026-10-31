package antigravity

import (
	"strconv"
	"strings"
	"testing"
)

// The upstream gates model visibility and quota granularity on the advertised
// client version. Everything that talks to CloudCode must therefore agree on one
// value, and that value must not slide backwards: the probe once sat on 1.11.5
// while the executor sent 1.104.0, and the probe was served a flattened quota
// view as a result.
const minimumClientVersion = "2.22.0"

func TestClientVersionIsNotBehindTheKnownFloor(t *testing.T) {
	if compareVersions(t, ClientVersion, minimumClientVersion) < 0 {
		t.Fatalf("ClientVersion = %s, must not be older than %s", ClientVersion, minimumClientVersion)
	}
}

// The header format is what the real Hub sends, and the version has to appear
// in it, or the upstream cannot read it.
func TestClientUserAgentCarriesTheVersion(t *testing.T) {
	if !strings.HasPrefix(ClientUserAgent, "antigravity/hub/") {
		t.Fatalf("ClientUserAgent = %q, want the antigravity/hub/<version> form", ClientUserAgent)
	}
	if !strings.Contains(ClientUserAgent, ClientVersion) {
		t.Fatalf("ClientUserAgent = %q, does not carry ClientVersion %q", ClientUserAgent, ClientVersion)
	}
}

// The OAuth identity is deliberately a different string: token and userinfo
// calls go to Google's OAuth endpoints, not to CloudCode. Collapsing the two
// would send the wrong client to one of them.
func TestOAuthUserAgentStaysSeparateFromTheClientIdentity(t *testing.T) {
	if APIUserAgent == ClientUserAgent {
		t.Fatal("APIUserAgent and ClientUserAgent must stay distinct")
	}
}

func compareVersions(t *testing.T, left, right string) int {
	t.Helper()
	leftParts, rightParts := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		a, b := 0, 0
		if i < len(leftParts) {
			a = mustAtoi(t, leftParts[i])
		}
		if i < len(rightParts) {
			b = mustAtoi(t, rightParts[i])
		}
		if a != b {
			if a < b {
				return -1
			}
			return 1
		}
	}
	return 0
}

func mustAtoi(t *testing.T, raw string) int {
	t.Helper()
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("version segment %q is not numeric: %v", raw, err)
	}
	return value
}
