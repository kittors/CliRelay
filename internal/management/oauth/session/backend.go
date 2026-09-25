package session

import "time"

// Backend is the session state an OAuth login flow drives. Store keeps it in
// process memory and hands callbacks over through files in the auth
// directory; ClusterStore shares it through the database so that a callback
// or a status poll may land on any node of a cluster.
type Backend interface {
	RegisterTenant(state, provider, tenantID string)
	SetError(state, message string)
	Complete(state string)
	CompleteProviderTenant(provider, tenantID string) int
	Get(state string) (Session, bool)
	IsPending(state, provider string) bool
	// DeliverCallback hands a provider callback to the pending session state.
	// It returns ErrNotPending when no pending session can take it.
	DeliverCallback(authDir, provider, state, code, errorMessage string) error
	// WaitCallback blocks until the callback for state arrives, the session
	// stops being pending (ErrNotPending) or timeout passes.
	WaitCallback(authDir, provider, state string, timeout, pollInterval time.Duration) (map[string]string, error)
}

var (
	_ Backend = (*Store)(nil)
	_ Backend = (*ClusterStore)(nil)
)

// DeliverCallback implements Backend by writing the callback file the waiting
// flow polls for.
func (s *Store) DeliverCallback(authDir, provider, state, code, errorMessage string) error {
	_, err := s.WriteCallbackFileForPending(authDir, provider, state, code, errorMessage)
	return err
}

// WaitCallback implements Backend by polling for the callback file.
func (s *Store) WaitCallback(authDir, provider, state string, timeout, pollInterval time.Duration) (map[string]string, error) {
	return s.WaitCallbackFile(authDir, provider, state, timeout, pollInterval)
}
