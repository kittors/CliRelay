package clusterauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// credentialWrite is one save, reduced to what the table stores.
type credentialWrite struct {
	id       string
	content  []byte
	runtime  map[string][]byte
	provider string
	// version is the version the change was based on.
	version int64
}

type saveDecision int

const (
	// decisionRuntime: the credential is unchanged; only runtime keys count.
	decisionRuntime saveDecision = iota
	// decisionWrite: the credential changed on top of the current version.
	decisionWrite
	// decisionStale: an edit based on a version that has been superseded.
	decisionStale
	// decisionGone: the credential is deleted or this node never saw it.
	decisionGone
)

// Save persists auth.
//
// An explicit create (coreauth.WithCredentialCreate: upload, login, import)
// inserts or replaces the credential, reviving a tombstone. Any other save is
// compared with the version it carries: unchanged credentials only buffer
// their runtime keys, changed ones are compare-and-set, and saves for deleted
// or unknown credentials, or based on a superseded version, are refused
// rather than allowed to overwrite newer data. A save recording a request
// result never waits on the database.
func (s *Store) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", errors.New("cluster auth store: auth is nil")
	}
	id, err := s.idForAuth(auth)
	if err != nil {
		return "", err
	}
	doc, err := persistedDocument(auth)
	if err != nil {
		return "", fmt.Errorf("cluster auth store: encode %s: %w", id, err)
	}
	content, runtime := splitDocument(doc)
	w := credentialWrite{id: id, version: coreauth.CredentialVersion(auth)}
	if w.content, err = s.encodeContent(id, content); err != nil {
		return "", fmt.Errorf("cluster auth store: encode %s: %w", id, err)
	}
	if w.runtime, err = canonicalEntries(runtime); err != nil {
		return "", fmt.Errorf("cluster auth store: encode %s: %w", id, err)
	}
	mirror := s.mirrorPath(id)
	if coreauth.IsCredentialCreate(ctx) {
		w.provider = documentProvider(content)
		return mirror, s.createCredential(ctx, w)
	}
	switch s.classify(w) {
	case decisionRuntime:
		return mirror, nil
	case decisionGone:
		return "", fmt.Errorf("%w: %s", coreauth.ErrCredentialGone, id)
	case decisionStale:
		return "", fmt.Errorf("%w: %s changed after version %d", coreauth.ErrCredentialConflict, id, w.version)
	}
	w.provider = documentProvider(content)
	if coreauth.IsResultPersist(ctx) {
		// markResult holds the manager's global lock; write in the background.
		s.deferWrite(w)
		return mirror, nil
	}
	return mirror, s.updateCredential(ctx, w)
}

// encodeContent returns the canonical encoding of a credential document.
// Saves run on every request, and a document decoded from JSON already
// encodes canonically, so the single-pass encoding is tried against this
// node's view first and the full round trip is only paid on a mismatch.
func (s *Store) encodeContent(id string, content map[string]any) ([]byte, error) {
	quick, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	entry := s.known[id]
	same := entry != nil && bytes.Equal(entry.content, quick)
	s.mu.Unlock()
	if same {
		return quick, nil
	}
	return canonicalJSON(content)
}

func (s *Store) classify(w credentialWrite) saveDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.known[w.id]
	if entry == nil || entry.deleted {
		return decisionGone
	}
	// Runtime keys are this node's observations whatever the credential's
	// version, so they are kept even from a stale snapshot.
	s.stageRuntimeLocked(w.id, entry, w.runtime)
	if bytes.Equal(entry.content, w.content) {
		return decisionRuntime
	}
	if w.version < entry.version {
		// A snapshot taken before another node's write, but not edited since,
		// is harmless: the mirror update will replace it on this node.
		if digest, ok := entry.history[w.version]; ok && digest == contentDigest(w.content) {
			return decisionRuntime
		}
		return decisionStale
	}
	return decisionWrite
}

// createCredential inserts or replaces id on an explicit create.
func (s *Store) createCredential(ctx context.Context, w credentialWrite) error {
	b, err := s.backendFor(ctx)
	if err != nil {
		return err
	}
	runtime, err := runtimeObject(w.runtime)
	if err != nil {
		return err
	}
	r := row{
		ID:        w.id,
		TenantID:  coreauth.TenantIDFromAuthID(w.id),
		FileName:  path.Base(w.id),
		AuthIndex: coreauth.FileAuthIndex(w.id),
		Provider:  w.provider,
		Content:   w.content,
		Runtime:   runtime,
		UpdatedBy: s.nodeID(),
	}
	stored, err := b.upsert(ctx, r, s.publisher(ctx, w.id, false))
	if err != nil {
		return fmt.Errorf("cluster auth store: %w", err)
	}
	// Buffered state belongs to the credential this create replaced.
	s.dropPending(w.id)
	r.Version, r.AuthIndex, r.CreatedAt, r.UpdatedAt = stored.Version, stored.AuthIndex, stored.CreatedAt, stored.UpdatedAt
	s.applyRow(r)
	return nil
}

// updateCredential compare-and-sets a changed credential.
func (s *Store) updateCredential(ctx context.Context, w credentialWrite) error {
	return s.compareAndSwap(ctx, w, true)
}

func (s *Store) compareAndSwap(ctx context.Context, w credentialWrite, mayRepair bool) error {
	b, err := s.backendFor(ctx)
	if err != nil {
		return err
	}
	r := row{ID: w.id, FileName: path.Base(w.id), Provider: w.provider, Content: w.content, UpdatedBy: s.nodeID()}
	version, ok, err := b.compareAndSwap(ctx, w.id, w.version, r, s.publisher(ctx, w.id, false))
	if err != nil {
		return fmt.Errorf("cluster auth store: %w", err)
	}
	if ok {
		s.recordWrite(w.id, version, w.content)
		return nil
	}
	return s.resolveConflict(ctx, b, w, mayRepair)
}

// resolveConflict handles a compare-and-set that did not apply. The newest
// row always becomes this node's view (and mirror); the write only counts
// as done when that row already holds the same credential.
func (s *Store) resolveConflict(ctx context.Context, b backend, w credentialWrite, mayRepair bool) error {
	current, err := b.get(ctx, w.id)
	if err != nil {
		return fmt.Errorf("cluster auth store: re-read %s after a version conflict: %w", w.id, err)
	}
	if current == nil {
		s.forget(w.id)
		return fmt.Errorf("%w: %s", coreauth.ErrCredentialGone, w.id)
	}
	if current.Deleted {
		s.applyRow(*current)
		return fmt.Errorf("%w: %s", coreauth.ErrCredentialGone, w.id)
	}
	if current.Version < w.version && mayRepair {
		// The database is behind a version this node saw committed: a
		// failover dropped the last writes. Re-apply this one on top of what
		// survived instead of letting the older tokens win.
		log.Warnf("cluster auth: %s is at version %d in the database but version %d was committed before; re-applying the newer credential", w.id, current.Version, w.version)
		retry := w
		retry.version = current.Version
		s.mu.Lock()
		s.recordRowLocked(*current, true)
		s.mu.Unlock()
		defer s.syncMirror(w.id)
		return s.compareAndSwap(ctx, retry, false)
	}
	s.applyRow(*current)
	if latest, errCanon := canonicalObject(current.Content); errCanon == nil && bytes.Equal(latest, w.content) {
		return nil
	}
	return fmt.Errorf("%w: %s is at version %d, the write was based on version %d", coreauth.ErrCredentialConflict, w.id, current.Version, w.version)
}

func (s *Store) idForAuth(auth *coreauth.Auth) (string, error) {
	candidates := []string{auth.ID}
	if strings.TrimSpace(auth.ID) == "" {
		if auth.Attributes != nil {
			candidates = append(candidates, auth.Attributes["path"])
		}
		candidates = append(candidates, auth.FileName)
	}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) != "" {
			return s.normalizeID(candidate)
		}
	}
	return "", errors.New("cluster auth store: credential has no ID")
}

func runtimeObject(entries map[string][]byte) ([]byte, error) {
	obj := make(map[string]json.RawMessage, len(entries))
	for key, raw := range entries {
		obj[key] = raw
	}
	return json.Marshal(obj)
}

func canonicalObject(raw []byte) ([]byte, error) {
	obj, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(obj)
}
