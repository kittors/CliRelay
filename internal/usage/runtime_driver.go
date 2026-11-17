package usage

// RuntimeDriver names the engine of the runtime database: "postgres" or
// "sqlite", empty before it is opened. The node's database probe
// (nodemetrics.DatabaseProbe) only speaks PostgreSQL.
func RuntimeDriver() string {
	usageDBMu.Lock()
	defer usageDBMu.Unlock()
	return usageDriver
}

// UsageSpoolDir returns the directory of the usage spool, empty while
// spooling is off. Unlike GetUsageWriteStatus it reads no files, so the node
// snapshot can ask for it every few seconds.
func UsageSpoolDir() string {
	rt := usageSpoolCurrent.Load()
	if rt == nil || rt.spool == nil {
		return ""
	}
	return rt.spool.dir
}
