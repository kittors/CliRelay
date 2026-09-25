package aiaccountstatus

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	log "github.com/sirupsen/logrus"
)

// JobSnapshotKind names refresh jobs in the shared snapshot store.
const JobSnapshotKind = "ai_account_status_refresh"

const (
	jobLookupTimeout = 5 * time.Second
	lostNodeCode     = "node_lost"
	lostNodeMessage  = "the server node running this refresh stopped before it finished"
)

// ShareJobSnapshots publishes refresh job progress through store, so the
// panel's polls can reach any node of a cluster. Call it before the first
// StartRefresh. Without it jobs are only visible on the node running them.
func (s *Service) ShareJobSnapshots(store jobsnapshot.Store, owner func() string) {
	if s == nil || store == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sharedJobs = jobsnapshot.NewPublisher(store, JobSnapshotKind, jobTTL, owner)
}

// shareJobLocked copies j for publishing and bumps its version. It returns a
// nil publisher when jobs are not shared.
func (s *Service) shareJobLocked(j *job) (*jobsnapshot.Publisher, jobsnapshot.Snapshot) {
	if s.sharedJobs == nil || j == nil {
		return nil, jobsnapshot.Snapshot{}
	}
	j.version++
	view := snapshotJobLocked(j)
	raw, err := json.Marshal(view)
	if err != nil {
		log.WithError(err).Warn("aiaccountstatus: failed to encode refresh job snapshot")
		return nil, jobsnapshot.Snapshot{}
	}
	return s.sharedJobs, jobsnapshot.Snapshot{
		ID:        j.ID,
		TenantID:  j.TenantID,
		Status:    j.State,
		Terminal:  j.State == "completed",
		Result:    raw,
		Version:   j.version,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
	}
}

func (s *Service) setResult(jobID, subjectID string, fn func(*AccountRefreshResult)) {
	s.mu.Lock()
	j := s.jobs[jobID]
	if j == nil {
		s.mu.Unlock()
		return
	}
	r := j.Results[subjectID]
	if r == nil {
		r = &AccountRefreshResult{AuthSubjectID: subjectID}
		j.Results[subjectID] = r
	}
	fn(r)
	j.UpdatedAt = time.Now().UTC()
	shared, snap := s.shareJobLocked(j)
	s.mu.Unlock()
	shared.Publish(snap)
}

// finishJob marks a job completed once every account has finished.
func (s *Service) finishJob(jobID string) {
	s.mu.Lock()
	j := s.jobs[jobID]
	if j == nil {
		s.mu.Unlock()
		return
	}
	j.State = "completed"
	j.UpdatedAt = time.Now().UTC()
	shared, snap := s.shareJobLocked(j)
	s.mu.Unlock()
	shared.Publish(snap)
}

func (s *Service) GetJob(tenantID, jobID string) (JobSnapshot, bool) {
	s.purgeExpiredJobs()
	s.mu.Lock()
	j := s.jobs[jobID]
	if j != nil && j.TenantID == strings.TrimSpace(tenantID) {
		snap := snapshotJobLocked(j)
		s.mu.Unlock()
		return snap, true
	}
	shared := s.sharedJobs
	s.mu.Unlock()
	return lookupSharedJob(shared, tenantID, jobID)
}

func snapshotJobLocked(j *job) JobSnapshot {
	snap := JobSnapshot{
		JobID:     j.ID,
		TenantID:  j.TenantID,
		State:     j.State,
		CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt,
		Results:   make([]AccountRefreshResult, 0, len(j.order)),
	}
	for _, sid := range j.order {
		if r := j.Results[sid]; r != nil {
			snap.Results = append(snap.Results, *r)
		}
	}
	countResults(&snap)
	return snap
}

func countResults(snap *JobSnapshot) {
	snap.Total, snap.Completed, snap.Failed = 0, 0, 0
	for _, r := range snap.Results {
		snap.Total++
		switch {
		case r.ErrorCode == "deduplicated" || r.ErrorCode == "fresh":
			snap.Completed++
		case r.State == RefreshSuccess:
			snap.Completed++
		case r.State == RefreshError:
			snap.Failed++
			snap.Completed++
		}
	}
}

// lookupSharedJob answers a poll for a job another node runs. A job whose
// node stopped heartbeating can no longer finish; its unfinished accounts are
// reported as failed so the panel stops waiting for them.
func lookupSharedJob(shared *jobsnapshot.Publisher, tenantID, jobID string) (JobSnapshot, bool) {
	if shared == nil {
		return JobSnapshot{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), jobLookupTimeout)
	defer cancel()
	stored, ok, err := shared.Lookup(ctx, jobID)
	if err != nil {
		log.WithError(err).WithField("job_id", jobID).Warn("aiaccountstatus: failed to read shared refresh job")
		return JobSnapshot{}, false
	}
	if !ok || stored.TenantID != strings.TrimSpace(tenantID) {
		return JobSnapshot{}, false
	}
	var snap JobSnapshot
	if err := json.Unmarshal(stored.Result, &snap); err != nil {
		log.WithError(err).WithField("job_id", jobID).Warn("aiaccountstatus: failed to decode shared refresh job")
		return JobSnapshot{}, false
	}
	if stored.OwnerLost {
		snap.State = "completed"
		for i := range snap.Results {
			if r := &snap.Results[i]; r.State == RefreshQueued || r.State == RefreshRunning {
				r.State = RefreshError
				r.ErrorCode = lostNodeCode
				r.ErrorMessage = lostNodeMessage
			}
		}
		countResults(&snap)
	}
	return snap, true
}

func (s *Service) purgeExpiredJobs() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// Drop stale success memory outside min-gap so the map stays bounded.
	for key, at := range s.lastSuccess {
		if now.Sub(at) >= accountRefreshMinGap {
			delete(s.lastSuccess, key)
		}
	}
	for id, j := range s.jobs {
		if now.Sub(j.UpdatedAt) > jobTTL {
			delete(s.jobs, id)
		}
	}
	for key, jobID := range s.inFlight {
		if _, ok := s.jobs[jobID]; !ok {
			delete(s.inFlight, key)
		}
	}
}
