package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// Scheduler is the job manager's view of the CGPU placement engine (cmd/cgpu-scheduler).
type Scheduler interface {
	Schedule(ctx context.Context, req *v1.PlacementRequest) (*v1.PlacementDecision, error)
	Release(ctx context.Context, jobID string) error
	Nodes(ctx context.Context) ([]v1.GPUNode, error)
	Pool(ctx context.Context) (*v1.PoolCapacitySummary, error)
	ContextReserveMiB() int
}

// SchedulerClient talks to the scheduler's HTTP API.
type SchedulerClient struct {
	BaseURL    string
	ReserveMiB int
	HTTP       *http.Client
}

func NewSchedulerClient(base string) *SchedulerClient {
	return &SchedulerClient{BaseURL: base, ReserveMiB: 512, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (c *SchedulerClient) ContextReserveMiB() int { return c.ReserveMiB }

func (c *SchedulerClient) do(ctx context.Context, method, path string, body, into interface{}) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("scheduler %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("scheduler %s %s: HTTP %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if into != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode, nil
}

func (c *SchedulerClient) Schedule(ctx context.Context, r *v1.PlacementRequest) (*v1.PlacementDecision, error) {
	var d v1.PlacementDecision
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/schedule", r, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Release frees the job's reservation, or cancels it if still queued. A 404 means the
// scheduler no longer holds anything for the job (already released), which is success.
func (c *SchedulerClient) Release(ctx context.Context, jobID string) error {
	code, err := c.do(ctx, http.MethodPost, "/api/v1/release", map[string]string{"job_id": jobID}, nil)
	if code == http.StatusNotFound {
		return nil
	}
	return err
}

func (c *SchedulerClient) Nodes(ctx context.Context) ([]v1.GPUNode, error) {
	var n []v1.GPUNode
	_, err := c.do(ctx, http.MethodGet, "/api/v1/nodes", nil, &n)
	return n, err
}

func (c *SchedulerClient) Pool(ctx context.Context) (*v1.PoolCapacitySummary, error) {
	var p v1.PoolCapacitySummary
	_, err := c.do(ctx, http.MethodGet, "/api/v1/pool", nil, &p)
	return &p, err
}

// AgentInventory mirrors the /inventory payload of cmd/cgpu-worker-agent.
type AgentInventory struct {
	NodeName  string `json:"node_name"`
	DriverVer string `json:"driver_version"`
	CUDAVer   string `json:"cuda_version"`
	GPUs      []struct {
		UUID           string  `json:"uuid"`
		MemoryUsedMiB  int     `json:"memory_used_mib"`
		UtilizationPct int     `json:"utilization_pct"`
		TemperatureC   int     `json:"temperature_c"`
		PowerDrawW     float64 `json:"power_draw_w"`
		Healthy        bool    `json:"healthy"`
	} `json:"gpus"`
	Jobs []struct {
		JobID     string `json:"job_id"`
		UsedBytes int64  `json:"used_bytes"`
		PeakBytes int64  `json:"peak_bytes"`
	} `json:"jobs"`
	CollectedAt time.Time `json:"collected_at"`
}

// AgentClient reads live telemetry from the worker agents (port 9400), caching briefly so
// a busy portal does not hammer the laptops. Failures are cached too and reported as
// "telemetry unavailable" rather than as zeros.
type AgentClient struct {
	Port int
	TTL  time.Duration
	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]agentEntry
}

type agentEntry struct {
	inv *AgentInventory
	err error
	at  time.Time
}

func NewAgentClient(port int) *AgentClient {
	return &AgentClient{Port: port, TTL: 5 * time.Second, HTTP: &http.Client{Timeout: 1500 * time.Millisecond}, cache: map[string]agentEntry{}}
}

func (a *AgentClient) Inventory(ctx context.Context, ip string) (*AgentInventory, error) {
	if a == nil || ip == "" {
		return nil, fmt.Errorf("no agent address")
	}
	a.mu.Lock()
	if e, ok := a.cache[ip]; ok && time.Since(e.at) < a.TTL {
		a.mu.Unlock()
		return e.inv, e.err
	}
	a.mu.Unlock()

	inv, err := a.fetch(ctx, ip)
	a.mu.Lock()
	a.cache[ip] = agentEntry{inv: inv, err: err, at: time.Now()}
	a.mu.Unlock()
	return inv, err
}

func (a *AgentClient) fetch(ctx context.Context, ip string) (*AgentInventory, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/inventory", ip, a.Port), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agent HTTP %d", resp.StatusCode)
	}
	var inv AgentInventory
	if err := json.NewDecoder(resp.Body).Decode(&inv); err != nil {
		return nil, err
	}
	return &inv, nil
}
