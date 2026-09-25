package imagegeneration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/management/jobsnapshot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	DefaultTaskTimeout = 5 * time.Minute
	DefaultTaskTTL     = 30 * time.Minute
)

type ExecuteFunc func(context.Context, string, []byte, string) ([]byte, error)

type Service struct {
	mu           sync.Mutex
	tasks        map[string]*task
	execute      ExecuteFunc
	timeout      time.Duration
	ttl          time.Duration
	systemAPIKey string
	now          func() time.Time
	// shared publishes snapshots for the other nodes of a cluster; nil on a
	// single node.
	shared *jobsnapshot.Publisher
}

type task struct {
	ID        string
	TenantID  string
	Status    string
	Phase     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Result    json.RawMessage
	Error     map[string]any
	version   int64
}

type Snapshot struct {
	ID        string
	Status    string
	Phase     string
	CreatedAt time.Time
	UpdatedAt time.Time
	ElapsedMs int64
	Result    any
	Error     map[string]any
}

type statusCoder interface {
	StatusCode() int
}

func NewService(execute ExecuteFunc, systemAPIKey string) *Service {
	return &Service{
		tasks:        make(map[string]*task),
		execute:      execute,
		timeout:      DefaultTaskTimeout,
		ttl:          DefaultTaskTTL,
		systemAPIKey: strings.TrimSpace(systemAPIKey),
		now:          time.Now,
	}
}

func (s *Service) Start(tenantID string, payload []byte, alt string) Snapshot {
	s.purgeExpired()
	now := s.now()
	item := &task{
		ID:        uuid.NewString(),
		TenantID:  strings.TrimSpace(tenantID),
		Status:    "queued",
		Phase:     "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.mu.Lock()
	if s.tasks == nil {
		s.tasks = make(map[string]*task)
	}
	s.tasks[item.ID] = item
	snapshot := s.snapshot(item)
	shared, sharedSnap := s.sharedSnapshotLocked(item)
	s.mu.Unlock()
	// Published before the id is returned, so the first poll finds the task
	// whichever node it reaches.
	shared.Publish(sharedSnap)

	go s.run(item.TenantID, item.ID, payload, alt)
	return snapshot
}

func (s *Service) Get(tenantID, taskID string) (Snapshot, bool) {
	s.mu.Lock()
	item := s.tasks[taskID]
	if item != nil && item.TenantID == strings.TrimSpace(tenantID) {
		snapshot := s.snapshot(item)
		s.mu.Unlock()
		return snapshot, true
	}
	s.mu.Unlock()
	return s.lookupShared(tenantID, taskID)
}

func (s *Service) run(tenantID, taskID string, payload []byte, alt string) {
	s.update(taskID, func(item *task) {
		item.Status = "running"
		item.Phase = "queued"
	})

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, util.ContextKeyTrustedTenantID, tenantID)
	ctx = context.WithValue(ctx, util.ContextKeyAPIKey, s.systemAPIKey)
	ctx = context.WithValue(ctx, util.ContextKeyImageGenerationPhaseHook, func(phase string) {
		s.update(taskID, func(item *task) {
			if strings.TrimSpace(phase) != "" {
				item.Phase = phase
			}
		})
	})

	result, err := s.execute(ctx, tenantID, payload, alt)
	if err != nil {
		status := 502
		if statusErr, ok := err.(statusCoder); ok && statusErr.StatusCode() > 0 {
			status = statusErr.StatusCode()
		}
		s.update(taskID, func(item *task) {
			item.Status = "failed"
			item.Error = errorResponse(err, status, "upstream_error")
		})
		return
	}

	s.update(taskID, func(item *task) {
		item.Status = "succeeded"
		item.Phase = "completed"
		item.Result = append(json.RawMessage(nil), result...)
	})
}

func (s *Service) update(taskID string, update func(*task)) {
	if update == nil {
		return
	}
	s.mu.Lock()
	item := s.tasks[taskID]
	if item == nil {
		s.mu.Unlock()
		return
	}
	update(item)
	item.UpdatedAt = s.now()
	shared, sharedSnap := s.sharedSnapshotLocked(item)
	s.mu.Unlock()
	shared.Publish(sharedSnap)
}

func (s *Service) purgeExpired() {
	cutoff := s.now().Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.tasks {
		if item == nil || item.UpdatedAt.Before(cutoff) {
			delete(s.tasks, id)
		}
	}
}

func (s *Service) snapshot(item *task) Snapshot {
	if item == nil {
		return Snapshot{}
	}

	result := Snapshot{
		ID:        item.ID,
		Status:    item.Status,
		Phase:     item.Phase,
		CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt,
		ElapsedMs: s.now().Sub(item.CreatedAt).Milliseconds(),
	}
	if item.Result != nil {
		var decoded any
		if err := json.Unmarshal(item.Result, &decoded); err == nil {
			result.Result = decoded
		}
	}
	if item.Error != nil {
		result.Error = cloneMap(item.Error)
	}
	return result
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

type upstreamErrorBodyProvider interface {
	UpstreamErrorBody() []byte
}

func errorResponse(err error, status int, errorType string) map[string]any {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	if msg == "" {
		msg = "Upstream image generation request failed."
	}
	typ := strings.TrimSpace(errorType)
	if typ == "" {
		typ = "upstream_error"
	}

	errorBody := map[string]any{
		"message": msg,
		"type":    typ,
	}
	if upstreamErr, ok := err.(upstreamErrorBodyProvider); ok {
		upstreamBody := strings.TrimSpace(string(upstreamErr.UpstreamErrorBody()))
		if upstreamBody != "" {
			var decoded any
			if json.Unmarshal([]byte(upstreamBody), &decoded) == nil {
				errorBody["upstream"] = decoded
			} else {
				errorBody["upstream"] = upstreamBody
			}
		}
	}
	return map[string]any{
		"status": status,
		"body": map[string]any{
			"error": errorBody,
		},
	}
}
