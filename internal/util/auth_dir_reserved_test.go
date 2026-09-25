package util

import (
	"path/filepath"
	"testing"
)

func TestIsReservedAuthPath(t *testing.T) {
	root := filepath.Join("/", "srv", "auths")
	cases := map[string]bool{
		filepath.Join(root, "a.json"):                                                        false,
		filepath.Join(root, "11111111-1111-1111-1111-111111111111", "a.json"):                false,
		filepath.Join(root, PreClusterBackupDirPrefix+"20260924-120000", "a.json"):           true,
		filepath.Join(root, PreClusterBackupDirPrefix+"20260924-120000", "tenant", "a.json"): true,
		filepath.Join(root, ClusterStrayDirPrefix+"20260924-120000", "a.json"):               true,
		filepath.Join("/", "srv", PreClusterBackupDirPrefix+"x", "a.json"):                   false,
		root: false,
	}
	for path, want := range cases {
		if got := IsReservedAuthPath(root, path); got != want {
			t.Errorf("IsReservedAuthPath(%q) = %v, want %v", path, got, want)
		}
	}
}
