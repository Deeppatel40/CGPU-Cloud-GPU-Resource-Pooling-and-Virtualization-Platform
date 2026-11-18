package jobs

import (
	"context"
	"errors"
	"sort"
	"sync"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

var (
	// ErrDuplicateRequest is returned by Insert when request_id is already stored.
	ErrDuplicateRequest = errors.New("duplicate request_id")
	// ErrNotFound is returned when a job does not exist.
	ErrNotFound = errors.New("job not found")
)

// ListFilter selects jobs for the history view.
type ListFilter struct {
	Status     v1.JobState
	WorkloadID string
	Page       int // 1-based
	Limit      int
}

// Store persists jobs. Insert must enforce request_id uniqueness atomically so that two
// concurrent submissions with the same request_id produce exactly one job.
type Store interface {
	Insert(ctx context.Context, j *Job) error
	Get(ctx context.Context, id string) (*Job, error)
	GetByRequestID(ctx context.Context, requestID string) (*Job, error)
	Replace(ctx context.Context, j *Job) error
	List(ctx context.Context, f ListFilter) ([]*Job, int, error)
	ListActive(ctx context.Context) ([]*Job, error)
}

// MemoryStore is an in-process Store used by tests and for running without MongoDB.
type MemoryStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
	byRq map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{jobs: map[string]*Job{}, byRq: map[string]string{}}
}

func clone(j *Job) *Job {
	c := *j
	c.Artifacts = append([]v1.ArtifactMetadata(nil), j.Artifacts...)
	return &c
}

func (m *MemoryStore) Insert(_ context.Context, j *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byRq[j.RequestID]; ok {
		return ErrDuplicateRequest
	}
	m.jobs[j.ID] = clone(j)
	m.byRq[j.RequestID] = j.ID
	return nil
}

func (m *MemoryStore) Get(_ context.Context, id string) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return nil, ErrNotFound
	}
	return clone(j), nil
}

func (m *MemoryStore) GetByRequestID(ctx context.Context, rq string) (*Job, error) {
	m.mu.Lock()
	id, ok := m.byRq[rq]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return m.Get(ctx, id)
}

func (m *MemoryStore) Replace(_ context.Context, j *Job) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[j.ID]; !ok {
		return ErrNotFound
	}
	m.jobs[j.ID] = clone(j)
	return nil
}

func (m *MemoryStore) List(_ context.Context, f ListFilter) ([]*Job, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*Job
	for _, j := range m.jobs {
		if f.Status != "" && j.Status != f.Status {
			continue
		}
		if f.WorkloadID != "" && j.WorkloadID != f.WorkloadID {
			continue
		}
		all = append(all, clone(j))
	}
	sort.Slice(all, func(a, b int) bool {
		return all[a].Timestamps.SubmittedAt.After(all[b].Timestamps.SubmittedAt)
	})
	total := len(all)
	start := (f.Page - 1) * f.Limit
	if start > total {
		start = total
	}
	end := start + f.Limit
	if end > total {
		end = total
	}
	return all[start:end], total, nil
}

func (m *MemoryStore) ListActive(_ context.Context) ([]*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Job
	for _, j := range m.jobs {
		if !j.Terminal() {
			out = append(out, clone(j))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}
