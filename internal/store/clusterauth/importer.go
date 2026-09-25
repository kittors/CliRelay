package clusterauth

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// bootstrap runs under the import lock when a node starts. The first node of
// a cluster finds the table empty and imports its auth directory; every node
// then aligns its directory with the table. A node joining a cluster that
// already holds credentials imports nothing: its local files may be stale
// copies, or copies of credentials deleted since, and must not overwrite or
// resurrect anything. They are moved aside, not deleted.
func (s *Store) bootstrap(ctx context.Context) error {
	b, err := s.backendFor(ctx)
	if err != nil {
		return err
	}
	rows, err := b.count(ctx)
	if err != nil {
		return fmt.Errorf("count credentials: %w", err)
	}
	if rows == 0 {
		if err = s.importLocal(ctx, b); err != nil {
			return err
		}
	} else {
		log.Infof("cluster auth: the credential table already holds %d rows; local auth files in %s are not imported", rows, s.baseDir())
	}
	if err = s.loadKnown(ctx, b); err != nil {
		return err
	}
	s.reconcileLocalFiles(backupDirPrefix(), 0)
	return nil
}

// loadKnown reads the whole table into this node's view without touching the
// mirror; the caller aligns the directory afterwards.
func (s *Store) loadKnown(ctx context.Context, b backend) error {
	live, err := b.listLive(ctx)
	if err != nil {
		return fmt.Errorf("list credentials: %w", err)
	}
	versions, err := b.listVersions(ctx)
	if err != nil {
		return fmt.Errorf("list credential versions: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range live {
		s.recordRowLocked(r, true)
	}
	for _, v := range versions {
		if v.Deleted {
			s.recordRowLocked(row{ID: v.ID, Version: v.Version, Deleted: true}, true)
		}
	}
	return nil
}

// importLocal copies every credential file of the auth directory into the
// empty table, normalised the way the file store normalises it on load.
func (s *Store) importLocal(ctx context.Context, b backend) error {
	bindings, err := b.activeBindingIndexes(ctx)
	if err != nil {
		log.WithError(err).Warn("cluster auth: account bindings unavailable; auth_index is derived from the file path")
		bindings = nil
	}
	files := s.scanLocalFiles()
	rows := make([]row, 0, len(files))
	kept := 0
	for _, file := range files {
		doc := file.doc
		provider := sdkauth.InferAuthProvider(doc)
		doc = sdkauth.NormalizeAuthMetadata(doc, provider)
		content, runtime := splitDocument(doc)
		canonical, errContent := canonicalJSON(content)
		runtimeEntries, errRuntime := canonicalEntries(runtime)
		if errContent != nil || errRuntime != nil {
			log.Warnf("cluster auth: not importing unreadable auth file %s", file.id)
			continue
		}
		runtimeRaw, errRuntime := runtimeObject(runtimeEntries)
		if errRuntime != nil {
			continue
		}
		index, fromBinding := importAuthIndex(file.id, bindings)
		if fromBinding {
			kept++
		}
		modTime := file.modTime
		if modTime.IsZero() {
			modTime = time.Now()
		}
		rows = append(rows, row{
			ID:        file.id,
			TenantID:  coreauth.TenantIDFromAuthID(file.id),
			FileName:  path.Base(file.id),
			AuthIndex: index,
			Provider:  provider,
			Content:   canonical,
			Runtime:   runtimeRaw,
			UpdatedAt: modTime,
			UpdatedBy: s.nodeID(),
		})
	}
	inserted, err := b.insertImported(ctx, rows)
	if err != nil {
		return err
	}
	log.Infof("cluster auth: imported %d of %d local auth files from %s into the empty credential table", inserted, len(files), s.baseDir())
	if kept > 0 {
		log.Infof("cluster auth: %d imported credentials keep the auth_index their account bindings record (credentials added since the last restart)", kept)
	}
	return nil
}

// importAuthIndex picks the auth_index an imported credential keeps.
//
// A single node assigns it when a credential enters memory and never changes
// it afterwards. At startup the file store loads every file first, seeding
// the index from the relative path (FileAuthIndex). A credential added at
// runtime through an OAuth login is registered with its base name instead, so
// until the next restart it runs, and logs, under the base-name index. The
// account binding table records the index each credential last ran with; when
// it holds exactly that base-name variant, the credential keeps it, so its
// recent request logs and quota history stay linked. Anything else gets the
// startup index, which is what a restart in single-node mode would give it.
func importAuthIndex(id string, bindings map[string]string) (string, bool) {
	natural := coreauth.FileAuthIndex(id)
	bound := strings.TrimSpace(bindings[bindingKey(coreauth.TenantIDFromAuthID(id), id)])
	if bound != "" && bound != natural && bound == coreauth.FileAuthIndex(path.Base(id)) {
		return bound, true
	}
	return natural, false
}
