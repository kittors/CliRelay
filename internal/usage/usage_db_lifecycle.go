package usage

import (
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// CloseDB drains admitted usage publications before closing the runtime store.
// Callers must first stop producing requests; sustained publication can delay
// the quiescent point. Publications admitted after closing retain the existing
// no-database/spool behavior, not a promise of persistence while no store exists.
// Do not call CloseDB from a usage plugin or publication callback.
func CloseDB() {
	coreusage.WithDrainedDefault(func() {
		// Do not hold the lifecycle mutex while awaiting callbacks: a plugin
		// may finish by calling InitDB. Serialize only the actual close.
		usageDBLifecycleMu.Lock()
		defer usageDBLifecycleMu.Unlock()
		closeUsageDBDrained()
	})
}

func closeUsageDBDrained() {
	// Stop maintenance before taking a write lease or usageDBMu: a worker may
	// need either to finish. Never wait for the dispatcher under these locks.
	stopRequestLogMaintenance()
	usageWriteLease.Lock()
	defer usageWriteLease.Unlock()

	usageDBMu.Lock()
	db, readDB := usageDB, usageReadDB
	usageDB, usageReadDB = nil, nil
	usageDBMu.Unlock()
	if db != nil {
		_ = db.Close()
	}
	if readDB != nil && readDB != db {
		_ = readDB.Close()
	}
	usageDBMetadataByHandle.Delete(db)
	usageDBMetadataByHandle.Delete(readDB)
	usageDBMu.Lock()
	usageLoc = nil
	usageDriver = ""
	usageDBMu.Unlock()
	resetAIAccountSubjectCycleCache()
	log.Info("usage: database closed")
}
