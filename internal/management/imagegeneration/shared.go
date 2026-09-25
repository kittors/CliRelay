package imagegeneration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	log "github.com/sirupsen/logrus"
)

const sharedLookupTimeout = 5 * time.Second

// maxSharedResultBytes is jobsnapshot.MaxResultBytes; tests lower it.
var maxSharedResultBytes = jobsnapshot.MaxResultBytes

var errOwnerLost = errors.New("the server node running this task stopped before it finished; run it again")

// ShareSnapshots publishes task snapshots through store, so a poll that
// reaches another node of a cluster still finds the task. Call it before the
// first Start. Without it tasks are only visible on the node that runs them.
func (s *Service) ShareSnapshots(store jobsnapshot.Store, kind string, owner func() string) {
	if s == nil || store == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shared = jobsnapshot.NewPublisher(store, kind, s.ttl, owner)
}

// sharedSnapshotLocked copies item for publishing and bumps its version, so
// the copies of one task are ordered however their writes interleave. It
// returns a nil publisher when tasks are not shared.
func (s *Service) sharedSnapshotLocked(item *task) (*jobsnapshot.Publisher, jobsnapshot.Snapshot) {
	if s.shared == nil || item == nil {
		return nil, jobsnapshot.Snapshot{}
	}
	item.version++
	snap := jobsnapshot.Snapshot{
		ID:        item.ID,
		TenantID:  item.TenantID,
		Status:    item.Status,
		Phase:     item.Phase,
		Terminal:  item.Status == "succeeded" || item.Status == "failed",
		Version:   item.version,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
	}
	if item.Result != nil {
		if len(item.Result) <= maxSharedResultBytes {
			snap.Result = item.Result
		} else {
			// The node that ran the task still serves the full result from
			// memory; the other nodes say why they cannot.
			snap.Status = "failed"
			snap.Error, _ = json.Marshal(errorResponse(
				fmt.Errorf("the result is %d bytes, over the %d bytes one task may share between server nodes; request fewer or smaller outputs", len(item.Result), maxSharedResultBytes),
				http.StatusBadGateway, "result_too_large"))
		}
	}
	if item.Error != nil && snap.Error == nil {
		snap.Error, _ = json.Marshal(item.Error)
	}
	return s.shared, snap
}

// lookupShared answers a poll for a task this node does not hold.
func (s *Service) lookupShared(tenantID, taskID string) (Snapshot, bool) {
	s.mu.Lock()
	shared := s.shared
	s.mu.Unlock()
	if shared == nil {
		return Snapshot{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), sharedLookupTimeout)
	defer cancel()
	snap, ok, err := shared.Lookup(ctx, taskID)
	if err != nil {
		log.WithError(err).WithField("task_id", taskID).Warn("imagegeneration: failed to read shared task snapshot")
		return Snapshot{}, false
	}
	if !ok || snap.TenantID != strings.TrimSpace(tenantID) {
		return Snapshot{}, false
	}
	out := Snapshot{
		ID:        snap.ID,
		Status:    snap.Status,
		Phase:     snap.Phase,
		CreatedAt: snap.CreatedAt,
		UpdatedAt: snap.UpdatedAt,
		ElapsedMs: s.now().Sub(snap.CreatedAt).Milliseconds(),
	}
	if snap.OwnerLost {
		out.Status = "failed"
		out.Error = errorResponse(errOwnerLost, http.StatusServiceUnavailable, "node_unavailable")
		return out, true
	}
	if len(snap.Result) > 0 {
		var decoded any
		if err := json.Unmarshal(snap.Result, &decoded); err == nil {
			out.Result = decoded
		}
	}
	if len(snap.Error) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal(snap.Error, &decoded); err == nil {
			out.Error = decoded
		}
	}
	return out, true
}
