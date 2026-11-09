package scheduler

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

func createTestCluster() *PlacementEngine {
	engine := NewPlacementEngine()

	// 1. RTX 4050 Node (Deep's WSL Worker)
	engine.RegisterOrUpdateNode(&v1.GPUNode{
		NodeID:         "wsl-rtx4050",
		KubernetesNode: "cgpu-gpu-worker-4050",
		Status:         v1.NodeOnline,
		Labels: map[string]string{
			"cgpu.io/role":      "gpu-worker",
			"cgpu.io/gpu-model": "rtx4050",
		},
		GPUs: []v1.GPUDevice{
			{
				GPUID:             "GPU-04280514-80a3-1dcf-1c5e-94e374365bef",
				Index:             0,
				Name:              "NVIDIA GeForce RTX 4050 Laptop GPU",
				TotalVRAMMiB:      6141,
				FreeVRAMMiB:       6141,
				AllocatedVRAMMiB:  0,
				HealthStatus:      v1.HealthHealthy,
				ActiveAllocations: []v1.ActiveAllocation{},
			},
		},
	})

	// 2. RTX 3050 Node (Future worker)
	engine.RegisterOrUpdateNode(&v1.GPUNode{
		NodeID:         "worker-rtx3050",
		KubernetesNode: "cgpu-gpu-worker-3050",
		Status:         v1.NodeOnline,
		Labels: map[string]string{
			"cgpu.io/role":      "gpu-worker",
			"cgpu.io/gpu-model": "rtx3050",
		},
		GPUs: []v1.GPUDevice{
			{
				GPUID:             "GPU-3050-dummy-uuid",
				Index:             0,
				Name:              "NVIDIA GeForce RTX 3050 GPU",
				TotalVRAMMiB:      4096,
				FreeVRAMMiB:       4096,
				AllocatedVRAMMiB:  0,
				HealthStatus:      v1.HealthHealthy,
				ActiveAllocations: []v1.ActiveAllocation{},
			},
		},
	})

	return engine
}

func TestBinpackingBestFit(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// Request 3000 MiB:
	// RTX 4050 has 6141 MiB -> remaining after = 3141 MiB
	// RTX 3050 has 4096 MiB -> remaining after = 1096 MiB
	// Best-fit MUST choose RTX 3050 because 1096 < 3141!
	req := &v1.PlacementRequest{
		JobID:            "job-binpack-1",
		WorkloadID:       "matrix-mult",
		RequestedVRAMMiB: 3000,
		Priority:         5,
		SubmittedAt:      time.Now().UTC(),
	}

	decision, err := engine.Schedule(ctx, req)
	if err != nil {
		t.Fatalf("unexpected schedule error: %v", err)
	}

	if decision.Status != v1.DecisionPlaced {
		t.Fatalf("expected PLACED, got %s", decision.Status)
	}

	if decision.AssignedNode != "cgpu-gpu-worker-3050" {
		t.Errorf("best-fit failed: expected cgpu-gpu-worker-3050, got %s", decision.AssignedNode)
	}

	if decision.AssignedGPUID != "GPU-3050-dummy-uuid" {
		t.Errorf("expected RTX 3050 GPU ID, got %s", decision.AssignedGPUID)
	}
}

func TestAtomicConcurrentReservations(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// RTX 4050 has 6141 MiB total.
	// Submit 7 concurrent requests of 1000 MiB each specifically targeting the 4050.
	// Exactly 6 requests must succeed (6 * 1000 = 6000 <= 6141),
	// and exactly 1 request must be queued as PENDING!
	var wg sync.WaitGroup
	results := make([]*v1.PlacementDecision, 7)
	errors := make([]error, 7)

	for i := 0; i < 7; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := &v1.PlacementRequest{
				JobID:            fmt.Sprintf("job-concurrent-%d", idx),
				WorkloadID:       "vector-add",
				RequestedVRAMMiB: 1000,
				Priority:         idx + 1, // higher index = higher priority
				NodeSelector: map[string]string{
					"cgpu.io/gpu-model": "rtx4050",
				},
				SubmittedAt: time.Now().UTC(),
			}
			results[idx], errors[idx] = engine.Schedule(ctx, req)
		}(i)
	}
	wg.Wait()

	placedCount := 0
	pendingCount := 0

	for i := 0; i < 7; i++ {
		if errors[i] != nil {
			t.Fatalf("job %d returned error: %v", i, errors[i])
		}
		if results[i].Status == v1.DecisionPlaced {
			placedCount++
		} else if results[i].Status == v1.DecisionPending {
			pendingCount++
		}
	}

	if placedCount != 6 {
		t.Errorf("expected exactly 6 placed jobs, got %d", placedCount)
	}
	if pendingCount != 1 {
		t.Errorf("expected exactly 1 pending job, got %d", pendingCount)
	}

	// Verify total allocated VRAM on RTX 4050 is exactly 6000 MiB
	cap, err := engine.GetPoolCapacity(ctx)
	if err != nil {
		t.Fatalf("failed to get pool capacity: %v", err)
	}
	if cap.AllocatedVRAMMiB != 6000 {
		t.Errorf("expected 6000 allocated VRAM across cluster, got %d", cap.AllocatedVRAMMiB)
	}
}

func TestCapacityReleaseAndQueueDispatch(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// Fill RTX 3050 (4096 MiB) with 4000 MiB job
	req1 := &v1.PlacementRequest{
		JobID:            "job-big",
		WorkloadID:       "heavy-workload",
		RequestedVRAMMiB: 4000,
		Priority:         5,
		NodeSelector: map[string]string{
			"cgpu.io/gpu-model": "rtx3050",
		},
		SubmittedAt: time.Now().UTC(),
	}
	d1, err := engine.Schedule(ctx, req1)
	if err != nil || d1.Status != v1.DecisionPlaced {
		t.Fatalf("failed to place req1: %v, status: %v", err, d1.Status)
	}

	// Second request for 2000 MiB on RTX 3050 must go PENDING (only 96 MiB left)
	req2 := &v1.PlacementRequest{
		JobID:            "job-waiting",
		WorkloadID:       "waiting-workload",
		RequestedVRAMMiB: 2000,
		Priority:         10,
		NodeSelector: map[string]string{
			"cgpu.io/gpu-model": "rtx3050",
		},
		SubmittedAt: time.Now().UTC(),
	}
	d2, err := engine.Schedule(ctx, req2)
	if err != nil || d2.Status != v1.DecisionPending {
		t.Fatalf("expected req2 PENDING, got %v", d2.Status)
	}

	if engine.GetPendingQueueLength() != 1 {
		t.Fatalf("expected queue length 1, got %d", engine.GetPendingQueueLength())
	}

	// Release job-big!
	err = engine.Release(ctx, "job-big", "worker-rtx3050", "GPU-3050-dummy-uuid", 4000)
	if err != nil {
		t.Fatalf("release error: %v", err)
	}

	// Pop next pending -> req2 should now be unblocked and placed!
	poppedReq, poppedDecision, err := engine.PopNextPending(ctx)
	if err != nil {
		t.Fatalf("pop error: %v", err)
	}
	if poppedReq == nil || poppedDecision == nil {
		t.Fatalf("expected popped job, got nil")
	}
	if poppedReq.JobID != "job-waiting" {
		t.Errorf("expected job-waiting, got %s", poppedReq.JobID)
	}
	if poppedDecision.Status != v1.DecisionPlaced {
		t.Errorf("expected PLACED after release, got %s", poppedDecision.Status)
	}
	if engine.GetPendingQueueLength() != 0 {
		t.Errorf("expected queue length 0, got %d", engine.GetPendingQueueLength())
	}
}

func TestHardwareExceededRejection(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// Largest GPU is RTX 4050 (6141 MiB).
	// Request 12000 MiB -> MUST reject immediately!
	req := &v1.PlacementRequest{
		JobID:            "job-impossible",
		WorkloadID:       "giant-llm",
		RequestedVRAMMiB: 12000,
		Priority:         10,
		SubmittedAt:      time.Now().UTC(),
	}

	decision, err := engine.Schedule(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if decision.Status != v1.DecisionRejected {
		t.Fatalf("expected REJECTED, got %s", decision.Status)
	}
	if decision.RejectionReason != v1.RejectionExceedsMaxHardware {
		t.Errorf("expected RejectionExceedsMaxHardware, got %s", decision.RejectionReason)
	}
}
