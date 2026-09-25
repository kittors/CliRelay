package util

import (
	"os"
	"path/filepath"
)

// StateDir picks the directory for process state that has to survive a restart,
// such as the request log spool and the last model list each credential fetched.
// The order prefers persistent locations:
//
//  1. WRITABLE_PATH, the base the log directory already uses when set;
//  2. a "data" directory next to the auth directory, when it exists. The Docker
//     image mounts /CLIProxyAPI/data as a volume, while /CLIProxyAPI itself
//     belongs to the container and is lost when it is recreated;
//  3. the auth directory's parent;
//  4. the working directory, reported as "".
//
// It is never the auth directory itself: the watcher reads every .json file
// there, subdirectories included, as a credential.
func StateDir(authDir string) string {
	if base := WritablePath(); base != "" {
		return base
	}
	resolved, err := ResolveAuthDir(authDir)
	if err != nil || resolved == "" {
		return ""
	}
	parent := filepath.Dir(resolved)
	if info, errStat := os.Stat(filepath.Join(parent, "data")); errStat == nil && info.IsDir() {
		return filepath.Join(parent, "data")
	}
	return parent
}
