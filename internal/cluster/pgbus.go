package cluster

import (
	"context"
	"errors"
)

// startPostgres builds the PostgreSQL-backed coordinator: the LISTEN
// connection, membership heartbeats and the leadership lock.
func startPostgres(_ context.Context, _ string, _ Options) (*Coordinator, error) {
	return nil, errors.New("postgres coordinator not implemented yet")
}
