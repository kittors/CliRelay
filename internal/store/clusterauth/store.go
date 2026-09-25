// Package clusterauth keeps credentials in PostgreSQL for cluster mode.
//
// Every node of a cluster serves the same accounts, so a credential must have
// one source of truth: the auth_credentials table. The local auth directory
// stays as a mirror of it, because the file watcher (and through it model
// registration), downloads and OAuth callbacks all work on files. Only this
// store writes mirror files; each one carries the version it mirrors.
//
// Writes come in two kinds. Request bookkeeping only touches runtime keys
// (health, quota gates, probe back-off); those are merged into the row
// asynchronously without a version bump, because markResult saves under the
// manager's global lock on every request. Credential changes (token
// refreshes, management edits, logins) are compare-and-set on the version and
// announced to the other nodes, which re-read the row and rewrite their
// mirror. A write based on an older version fails instead of overwriting a
// newer one, and a deleted credential stays deleted (a tombstone) until an
// explicit create brings it back.
package clusterauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cluster"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/storage/postgres/compatdriver"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	defaultReconcileInterval = 30 * time.Second
	defaultLocalScanInterval = 5 * time.Minute
	defaultFlushInterval     = time.Second
	defaultStrayGrace        = 2 * time.Minute
	defaultLeaseTTL          = 90 * time.Second
	// versionHistoryDepth is how many superseded versions a node remembers
	// the digest of, to tell a merely stale snapshot from a stale edit.
	versionHistoryDepth = 8
)

// ErrNotStarted is returned when the store is used before Start and has no
// DSN to connect with on its own.
var ErrNotStarted = errors.New("cluster auth store: not started")

// Options configures a Store.
type Options struct {
	// AuthDir is the local auth directory the store mirrors credentials into.
	AuthDir string
	// NodeID names this process in updated_by and in refresh leases. Empty
	// means the cluster coordinator's node ID.
	NodeID string
	// DSN lets one-shot CLI commands (logins, imports), which never call
	// Start, reach the database on their own.
	DSN string
	// Coordinator returns the cluster coordinator; nil means cluster.Default.
	Coordinator func() *cluster.Coordinator
	// ReconcileInterval is how often row versions are compared with this
	// node's view; events only speed convergence up, since NOTIFY is not
	// durable.
	ReconcileInterval time.Duration
	// LocalScanInterval is how often the whole auth directory is checked for
	// missing mirrors and stray files.
	LocalScanInterval time.Duration
	// FlushInterval is how often buffered runtime observations are written.
	FlushInterval time.Duration
	// StrayGrace is how long an unversioned file may sit in the auth
	// directory before a running node sets it aside; an upload writes its
	// file a moment before the store records it.
	StrayGrace time.Duration
	// LeaseTTL bounds how long a refresh lease outlives a crashed holder.
	LeaseTTL time.Duration
}

// knownEntry is this node's view of one row: the version it mirrors and the
// content it compares saves against.
type knownEntry struct {
	version   int64
	deleted   bool
	authIndex string
	content   []byte
	runtime   map[string][]byte
	history   map[int64][32]byte
}

// Store is the cluster-mode token store. It implements coreauth.Store,
// coreauth.VersionedStore, coreauth.AuthIndexResolver and
// coreauth.RefreshCoordinator.
type Store struct {
	opts Options

	dirMu   sync.RWMutex
	authDir string

	bindMu  sync.Mutex
	backend backend

	mu      sync.Mutex
	known   map[string]*knownEntry
	patches map[string]*runtimePatch
	writes  map[string]*deferredWrite

	// mirrorMu serialises mirror file writes so an older version can never
	// land after a newer one.
	mirrorMu sync.Mutex

	syncMu      sync.Mutex
	syncPending map[string]syncRequest
	resync      bool
	syncSignal  chan struct{}
	flushSignal chan struct{}

	started     atomic.Bool
	stop        chan struct{}
	wg          sync.WaitGroup
	unsubscribe func()
}

var (
	_ coreauth.VersionedStore     = (*Store)(nil)
	_ coreauth.AuthIndexResolver  = (*Store)(nil)
	_ coreauth.RefreshCoordinator = (*Store)(nil)
)

// New returns a store for opts. It does not touch the database until Start,
// or until a CLI command saves through it with a DSN configured.
func New(opts Options) *Store {
	if opts.ReconcileInterval <= 0 {
		opts.ReconcileInterval = defaultReconcileInterval
	}
	if opts.LocalScanInterval <= 0 {
		opts.LocalScanInterval = defaultLocalScanInterval
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlushInterval
	}
	if opts.StrayGrace <= 0 {
		opts.StrayGrace = defaultStrayGrace
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultLeaseTTL
	}
	return &Store{
		opts:        opts,
		authDir:     strings.TrimSpace(opts.AuthDir),
		known:       make(map[string]*knownEntry),
		patches:     make(map[string]*runtimePatch),
		writes:      make(map[string]*deferredWrite),
		syncPending: make(map[string]syncRequest),
		syncSignal:  make(chan struct{}, 1),
		flushSignal: make(chan struct{}, 1),
		stop:        make(chan struct{}),
	}
}

// SetBaseDir updates the auth directory the store mirrors into.
func (s *Store) SetBaseDir(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	s.dirMu.Lock()
	s.authDir = dir
	s.dirMu.Unlock()
}

func (s *Store) baseDir() string {
	s.dirMu.RLock()
	defer s.dirMu.RUnlock()
	return s.authDir
}

func (s *Store) coordinator() *cluster.Coordinator {
	if s.opts.Coordinator != nil {
		if c := s.opts.Coordinator(); c != nil {
			return c
		}
	}
	return cluster.Default()
}

func (s *Store) nodeID() string {
	if id := strings.TrimSpace(s.opts.NodeID); id != "" {
		return id
	}
	return s.coordinator().NodeID()
}

// backendFor returns the bound backend. A store that was never started (a
// CLI login) opens its own connection when it has a DSN.
func (s *Store) backendFor(ctx context.Context) (backend, error) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	if s.backend != nil {
		return s.backend, nil
	}
	dsn := strings.TrimSpace(s.opts.DSN)
	if dsn == "" {
		return nil, ErrNotStarted
	}
	db, err := sql.Open(compatdriver.DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("cluster auth store: open database: %w", err)
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cluster auth store: reach database: %w", err)
	}
	s.backend = newPGBackend(db, true)
	return s.backend, nil
}

// Start binds the store to the runtime database, imports or reconciles the
// local auth directory, and starts following the other nodes. It must run
// before the auth manager loads credentials.
func (s *Store) Start(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("cluster auth store: cluster mode needs the PostgreSQL runtime database")
	}
	s.bindMu.Lock()
	s.backend = newPGBackend(db, false)
	s.bindMu.Unlock()
	return s.start(ctx)
}

func (s *Store) start(ctx context.Context) error {
	b, err := s.backendFor(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.baseDir()) == "" {
		return errors.New("cluster auth store: auth directory is not configured")
	}
	if !s.coordinator().Enabled() {
		log.Warn("cluster auth: the cluster coordinator is not running; credential changes reach the other nodes only through the periodic reconcile")
	}
	if err = b.withImportLock(ctx, s.bootstrap); err != nil {
		return fmt.Errorf("cluster auth store: bootstrap: %w", err)
	}
	s.unsubscribe = s.coordinator().Subscribe(cluster.TopicAuth, s.onEvent)
	s.started.Store(true)
	s.wg.Add(2)
	go s.syncLoop()
	go s.writeLoop()
	return nil
}

// Close stops following the cluster and writes buffered runtime
// observations and deferred credential writes, as far as ctx allows.
func (s *Store) Close(ctx context.Context) {
	if s.started.CompareAndSwap(true, false) {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		close(s.stop)
		s.wg.Wait()
		s.flush(ctx)
	}
	s.bindMu.Lock()
	b := s.backend
	s.bindMu.Unlock()
	if b != nil {
		_ = b.close()
	}
}

// List returns every live credential. It only reads the table: no mirror
// writes, no normalisation write-back, no network probes.
func (s *Store) List(ctx context.Context) ([]*coreauth.Auth, error) {
	b, err := s.backendFor(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := b.listLive(ctx)
	if err != nil {
		return nil, fmt.Errorf("cluster auth store: list credentials: %w", err)
	}
	out := make([]*coreauth.Auth, 0, len(rows))
	for _, r := range rows {
		auth, errBuild := authFromRow(r, s.mirrorPath(r.ID))
		if errBuild != nil {
			log.WithError(errBuild).Warnf("cluster auth: skipping unreadable credential %s", r.ID)
			continue
		}
		out = append(out, auth)
	}
	return out, nil
}

// Get returns the newest persisted copy of id, or nil when it does not exist
// or was deleted. Seeing a newer version converges this node's mirror too.
func (s *Store) Get(ctx context.Context, id string) (*coreauth.Auth, error) {
	id, err := s.normalizeID(id)
	if err != nil {
		return nil, err
	}
	b, err := s.backendFor(ctx)
	if err != nil {
		return nil, err
	}
	r, err := b.get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cluster auth store: read %s: %w", id, err)
	}
	if r == nil {
		return nil, nil
	}
	s.applyRow(*r)
	if r.Deleted {
		return nil, nil
	}
	return authFromRow(*r, s.mirrorPath(r.ID))
}

// Delete tombstones the credential and removes its mirror. id may also be
// the credential's file path, which is what the management API passes.
func (s *Store) Delete(ctx context.Context, id string) error {
	id, err := s.normalizeID(id)
	if err != nil {
		return err
	}
	b, err := s.backendFor(ctx)
	if err != nil {
		return err
	}
	version, deleted, err := b.tombstone(ctx, id, s.nodeID(), s.publisher(ctx, id, true))
	if err != nil {
		return fmt.Errorf("cluster auth store: %w", err)
	}
	s.dropPending(id)
	if deleted {
		s.applyRow(row{ID: id, Version: version, Deleted: true})
	}
	return nil
}

// ResolveAuthIndex returns the auth_index pinned for id, or the index a
// single node would give a new credential with that ID.
func (s *Store) ResolveAuthIndex(id string) string {
	s.mu.Lock()
	entry := s.known[id]
	s.mu.Unlock()
	if entry != nil && entry.authIndex != "" {
		return entry.authIndex
	}
	return coreauth.FileAuthIndex(id)
}

// ClaimRefresh takes the refresh lease of id for this node. The lease row
// comes back with the newest credential, which also converges the mirror.
func (s *Store) ClaimRefresh(ctx context.Context, id string) (*coreauth.Auth, bool, error) {
	id, err := s.normalizeID(id)
	if err != nil {
		return nil, false, err
	}
	b, err := s.backendFor(ctx)
	if err != nil {
		return nil, false, err
	}
	r, err := b.claimLease(ctx, id, s.nodeID(), s.opts.LeaseTTL)
	if err != nil {
		return nil, false, err
	}
	if r == nil {
		return nil, false, nil
	}
	s.applyRow(*r)
	latest, err := authFromRow(*r, s.mirrorPath(r.ID))
	if err != nil {
		s.ReleaseRefresh(ctx, id)
		return nil, false, err
	}
	return latest, true, nil
}

// ReleaseRefresh gives the lease back. A failure only delays other nodes
// until the lease expires.
func (s *Store) ReleaseRefresh(ctx context.Context, id string) {
	id, err := s.normalizeID(id)
	if err != nil {
		return
	}
	b, err := s.backendFor(ctx)
	if err != nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errRelease := b.releaseLease(releaseCtx, id, s.nodeID()); errRelease != nil {
		log.WithError(errRelease).Debugf("cluster auth: release refresh lease of %s", id)
	}
}

// publisher returns the in-transaction notification for a write to id.
func (s *Store) publisher(ctx context.Context, id string, deleted bool) publishFunc {
	return func(tx *sql.Tx, version int64) error {
		return s.coordinator().PublishTx(ctx, tx, cluster.TopicAuth, cluster.AuthEvent{ID: id, Version: version, Deleted: deleted})
	}
}
