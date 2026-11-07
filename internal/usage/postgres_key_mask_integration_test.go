package usage

import (
	"os"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/testutil/postgrestest"
)

// The production request log lives in PostgreSQL, where the key facet, the
// system-call predicate, the distribution grouping and the masking UPDATE are
// all evaluated by a different engine than the SQLite the unit tests use.
func TestPostgresRequestLogKeysAreMaskedAndReadByKeyID(t *testing.T) {
	dsn := os.Getenv("CLIRELAY_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("CLIRELAY_POSTGRES_TEST_DSN is not set")
	}
	postgrestest.LockSharedRuntimeDB(t, dsn)
	CloseDB()
	t.Cleanup(CloseDB)

	if err := InitPostgres(config.PostgresConfig{
		DSN:          dsn,
		MaxOpenConns: 4,
		MaxIdleConns: 1,
	}, config.RequestLogStorageConfig{StoreContent: true}, time.UTC); err != nil {
		t.Fatalf("InitPostgres() error = %v", err)
	}
	stopRequestLogMaintenance()

	for _, check := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"the write path stores a mask and the key id", assertRequestLogWriteStoresMaskAndKeyIDOnly},
		{"system calls are one facet entry and one filter", assertSystemCallsAreOneEntryInTheKeyFacetAndFilter},
		{"an unnamed key is labelled by its mask", assertKeyFacetLabelsAnUnnamedKeyByItsMask},
		{"rows that still hold a secret are returned masked", assertRowsThatStillHoldASecretAreReturnedMasked},
		{"the distribution keeps unattributed rows as their own slices", assertAPIKeyDistributionKeepsRowsWithoutAKeyIDAsTheirOwnSlices},
		{"the masking pass fills ids, masks and leaves labels", assertRequestLogKeyMaskingEndToEnd},
	} {
		// Each check starts from empty tables; the masking pass in particular
		// needs its done marker cleared.
		truncatePostgresRuntimeTables(t, getDB())
		t.Run(check.name, check.run)
	}
}
