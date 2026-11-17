package jobs

import (
	"context"
	"fmt"
	"sync"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// Inventory merges the scheduler's view (capacity, reservations, node online state) with
// live measurements from the worker agents. Measurements that cannot be read are marked
// UNAVAILABLE with a reason; they are never reported as zero.
func (m *Manager) Inventory(ctx context.Context) (*v1.GPUInventoryResponse, error) {
	nodes, err := m.Sched.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	invs := m.agentInventories(ctx, nodes)

	resp := &v1.GPUInventoryResponse{Nodes: make([]v1.GPUNode, 0, len(nodes))}
	for i, n := range nodes {
		inv, agentErr := invs[i].inv, invs[i].err
		for gi := range n.GPUs {
			g := &n.GPUs[gi]
			resp.TotalGPUs++
			resp.AggregateVRAMMiB += g.TotalVRAMMiB
			resp.AllocatedVRAMMiB += g.AllocatedVRAMMiB
			if n.Status == v1.NodeOnline && g.HealthStatus == v1.HealthHealthy {
				resp.FreeVRAMMiB += g.FreeVRAMMiB
			}
			g.Telemetry = gpuTelemetry(n, g.GPUID, inv, agentErr)
			if inv != nil {
				g.DriverVersion, g.CUDAVersion = inv.DriverVer, inv.CUDAVer
			}
		}
		resp.Nodes = append(resp.Nodes, n)
	}
	resp.TotalNodes = len(nodes)
	return resp, nil
}

type invResult struct {
	inv *AgentInventory
	err error
}

func (m *Manager) agentInventories(ctx context.Context, nodes []v1.GPUNode) []invResult {
	out := make([]invResult, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		if n.Status != v1.NodeOnline {
			out[i].err = fmt.Errorf("node is %s", n.Status)
			continue
		}
		wg.Add(1)
		go func(i int, ip string) {
			defer wg.Done()
			out[i].inv, out[i].err = m.Agents.Inventory(ctx, ip)
		}(i, n.IPAddress)
	}
	wg.Wait()
	return out
}

func gpuTelemetry(n v1.GPUNode, gpuID string, inv *AgentInventory, err error) *v1.GPUTelemetrySample {
	if inv == nil {
		msg := "worker agent unreachable"
		if err != nil {
			msg = err.Error()
		}
		return &v1.GPUTelemetrySample{Status: v1.TelemetryUnavailable, Reason: "AGENT_UNREACHABLE", Message: msg, SampledAt: nowUTC()}
	}
	for _, g := range inv.GPUs {
		if g.UUID == gpuID {
			util, mem, temp, pw := g.UtilizationPct, g.MemoryUsedMiB, g.TemperatureC, g.PowerDrawW
			return &v1.GPUTelemetrySample{Status: v1.TelemetryAvailable, GPUUtilizationPct: &util, MemoryUsedMiB: &mem,
				TemperatureCelsius: &temp, PowerWatts: &pw, SampledAt: inv.CollectedAt}
		}
	}
	return &v1.GPUTelemetrySample{Status: v1.TelemetryUnavailable, Reason: "GPU_NOT_REPORTED",
		Message: "agent on " + n.NodeID + " does not report this GPU", SampledAt: inv.CollectedAt}
}

// GPU returns one device by ID.
func (m *Manager) GPU(ctx context.Context, gpuID string) (*v1.GPUDevice, *APIError) {
	inv, err := m.Inventory(ctx)
	if err != nil {
		return nil, apiErr(502, v1.ErrCodeInternalServerError, "scheduler unavailable: "+err.Error())
	}
	for _, n := range inv.Nodes {
		for _, g := range n.GPUs {
			if g.GPUID == gpuID {
				return &g, nil
			}
		}
	}
	return nil, apiErr(404, v1.ErrCodeGPUNotFound, "GPU "+gpuID+" not found")
}

// Enrich adds request-time details to a job response: why a pending job is waiting and,
// for a running job, the live VRAM its guard reports and its GPU's utilisation.
func (m *Manager) Enrich(ctx context.Context, j *Job) *v1.JobStatusResponse {
	r := j.Response()
	switch {
	case j.Status == v1.StatePending && j.Placement == nil:
		r.PendingDetails = m.pendingDetails(ctx, j)
	case j.Status == v1.StateRunning && j.Placement != nil:
		r.Telemetry = m.jobTelemetry(ctx, j)
	}
	return r
}

func (m *Manager) pendingDetails(ctx context.Context, j *Job) *v1.PendingDetails {
	d := &v1.PendingDetails{RequiredVRAMMiB: j.RequestedVRAMMiB}
	nodes, err := m.Sched.Nodes(ctx)
	if err != nil {
		d.Message = "scheduler unreachable; the job will be placed when it is back (" + err.Error() + ")"
		return d
	}
	online, largest := 0, 0
	for _, n := range nodes {
		for _, g := range n.GPUs {
			d.PoolTotalVRAMMiB += g.TotalVRAMMiB
			d.PoolAllocatedVRAMMiB += g.AllocatedVRAMMiB
			if n.Status == v1.NodeOnline && g.HealthStatus == v1.HealthHealthy {
				online++
				d.AvailableVRAMMiB += g.FreeVRAMMiB
				if g.FreeVRAMMiB > largest {
					largest = g.FreeVRAMMiB
				}
			}
		}
	}
	switch {
	case online == 0:
		d.Message = "all GPU workers are offline; the job starts when one rejoins the cluster"
	default:
		d.Message = fmt.Sprintf("needs %d MiB on a single GPU; the largest free slice is %d MiB. The job starts when a running job releases enough VRAM.", j.RequestedVRAMMiB, largest)
	}
	return d
}

func (m *Manager) jobTelemetry(ctx context.Context, j *Job) *v1.GPUTelemetrySample {
	nodes, err := m.Sched.Nodes(ctx)
	if err != nil {
		return &v1.GPUTelemetrySample{Status: v1.TelemetryUnavailable, Reason: "SCHEDULER_UNREACHABLE", Message: err.Error(), SampledAt: nowUTC()}
	}
	for _, n := range nodes {
		if n.KubernetesNode != j.Placement.NodeID && n.NodeID != j.Placement.NodeID {
			continue
		}
		inv, err := m.Agents.Inventory(ctx, n.IPAddress)
		t := gpuTelemetry(n, j.Placement.GPUID, inv, err)
		if inv != nil {
			// Per-job VRAM comes from the allocation guard's status file, not the device total.
			t.MemoryUsedMiB = nil
			for _, ju := range inv.Jobs {
				if ju.JobID == j.ID {
					mib := int(ju.UsedBytes / (1024 * 1024))
					t.MemoryUsedMiB = &mib
				}
			}
			if t.MemoryUsedMiB == nil && t.Status == v1.TelemetryAvailable {
				t.Message = "per-job VRAM not reported yet by the allocation guard"
			}
		}
		return t
	}
	return &v1.GPUTelemetrySample{Status: v1.TelemetryUnavailable, Reason: "NODE_UNKNOWN", Message: "node " + j.Placement.NodeID + " not in scheduler inventory", SampledAt: nowUTC()}
}
