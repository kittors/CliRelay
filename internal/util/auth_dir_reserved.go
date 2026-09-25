package util

import (
	"path/filepath"
	"strings"
)

// Cluster mode keeps the auth directory as a mirror of the database. Local
// credential files it has to set aside are moved, never deleted, into
// subdirectories with these prefixes. Every reader of the auth directory skips
// them, in both modes, so a set-aside credential can never load as a live
// account again: not on this node, and not after switching back to single-node.
const (
	// PreClusterBackupDirPrefix names the directory that receives the local
	// credentials of a node joining a cluster whose database already holds
	// credentials.
	PreClusterBackupDirPrefix = ".pre-cluster-backup-"
	// ClusterStrayDirPrefix names the directory a running cluster node moves
	// credential files into when the database does not know them.
	ClusterStrayDirPrefix = ".cluster-stray-"
)

// IsReservedAuthSubdir reports whether a directory name inside the auth
// directory holds set-aside credentials.
func IsReservedAuthSubdir(name string) bool {
	return strings.HasPrefix(name, PreClusterBackupDirPrefix) || strings.HasPrefix(name, ClusterStrayDirPrefix)
}

// IsReservedAuthPath reports whether path lies inside a reserved directory of
// authDir.
func IsReservedAuthPath(authDir, path string) bool {
	if strings.TrimSpace(authDir) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	rel, err := filepath.Rel(authDir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if IsReservedAuthSubdir(part) {
			return true
		}
	}
	return false
}
