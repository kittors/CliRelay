package usage

import (
	"database/sql"
	"sync"
	"time"
)

// A store's SQL dialect and bucket timezone never change while that store is
// alive. Keeping them with its handle/transactions prevents both racy global
// reads and accidentally using a newly opened store's configuration.
// This registry has its own synchronization: bootstrap callers already hold
// usageDBMu, so looking up their explicit db must not acquire that mutex again.
type usageDBMetadata struct {
	driver string
	loc    *time.Location
}

var usageDBMetadataByHandle sync.Map // *sql.DB -> usageDBMetadata

// usageWriteLease pins the store across the full live/spool retry sequence.
// Init and Close take the exclusive side; no callback is invoked under a lease.
var usageWriteLease sync.RWMutex

func metadataForUsageDB(db *sql.DB) usageDBMetadata {
	if v, ok := usageDBMetadataByHandle.Load(db); ok {
		return v.(usageDBMetadata)
	}
	// An unknown/closed handle must never inherit the *current* store's data.
	return usageDBMetadata{loc: time.Local}
}

type usageMetadataTx struct {
	usageWriteTx
	metadata usageDBMetadata
}

func (t usageMetadataTx) usageMetadata() usageDBMetadata { return t.metadata }
func (t boundTx) usageMetadata() usageDBMetadata         { return t.metadata }

// Operations whose SQL or buckets depend on store configuration require a
// bound transaction. A plain *sql.Tx cannot silently inherit another store.
type usageMetadataWriteTx interface {
	usageWriteTx
	usageMetadata() usageDBMetadata
}
