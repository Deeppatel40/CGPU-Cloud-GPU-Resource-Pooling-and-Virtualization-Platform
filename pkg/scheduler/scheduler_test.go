package scheduler

import (
	"context"
	"fmt"
	"os"
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
				GPUID:             "GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6",
				Index:             0,
				Name:              "NVIDIA GeForce RTX 3050 Laptop GPU",
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

	if decision.AssignedGPUID != "GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6" {
		t.Errorf("expected RTX 3050 GPU ID, got %s", decision.AssignedGPUID)
	}
}

func TestAtomicConcurrentReservations(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// RTX 4050 has 6141 MiB total, 5629 MiB allocatable after the 512 MiB driver reserve.
	// Submit 7 concurrent requests of 1000 MiB each specifically targeting the 4050.
	// Exactly 5 requests must succeed (5 * 1000 = 5000 <= 5629),
	// and exactly 2 requests must be queued as PENDING!
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

	if placedCount != 5 {
		t.Errorf("expected exactly 5 placed jobs, got %d", placedCount)
	}
	if pendingCount != 2 {
		t.Errorf("expected exactly 2 pending jobs, got %d", pendingCount)
	}

	// Verify total allocated VRAM on RTX 4050 is exactly 5000 MiB
	cap, err := engine.GetPoolCapacity(ctx)
	if err != nil {
		t.Fatalf("failed to get pool capacity: %v", err)
	}
	if cap.AllocatedVRAMMiB != 5000 {
		t.Errorf("expected 5000 allocated VRAM across cluster, got %d", cap.AllocatedVRAMMiB)
	}
}

func TestCapacityReleaseAndQueueDispatch(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	// Fill RTX 3050 (3584 MiB allocatable) with a 3000 MiB job
	req1 := &v1.PlacementRequest{
		JobID:            "job-big",
		WorkloadID:       "heavy-workload",
		RequestedVRAMMiB: 3000,
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

	// Second request for 2000 MiB on RTX 3050 must go PENDING (only 584 MiB left)
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
	err = engine.Release(ctx, "job-big", "worker-rtx3050", "GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6", 3000)
	if err != nil {
		t.Fatalf("release error: %v", err)
	}

	// Release dispatches the queue itself: req2 is placed without a separate PopNextPending.
	d, ok := engine.GetJob("job-waiting")
	if !ok || d.Status != v1.DecisionPlaced {
		t.Fatalf("expected job-waiting PLACED after release, got %+v", d)
	}
	if engine.GetPendingQueueLength() != 0 {
		t.Errorf("expected queue length 0, got %d", engine.GetPendingQueueLength())
	}
	if req, _, _ := engine.PopNextPending(ctx); req != nil {
		t.Errorf("queue should already be drained, popped %s", req.JobID)
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

func TestScheduleIsIdempotentPerJobID(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()
	req := &v1.PlacementRequest{JobID: "retry-me", WorkloadID: "w", RequestedVRAMMiB: 1000, SubmittedAt: time.Now()}

	d1, err := engine.Schedule(ctx, req)
	if err != nil || d1.Status != v1.DecisionPlaced {
		t.Fatalf("first schedule: %v %v", err, d1)
	}
	d2, err := engine.Schedule(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d2.AssignedGPUID != d1.AssignedGPUID || d2.Status != v1.DecisionPlaced {
		t.Fatalf("retry returned a different decision: %+v vs %+v", d2, d1)
	}
	cap, _ := engine.GetPoolCapacity(ctx)
	if cap.AllocatedVRAMMiB != 1000 {
		t.Fatalf("retry double-reserved: allocated=%d, want 1000", cap.AllocatedVRAMMiB)
	}
}

func TestMissingJobIDRejected(t *testing.T) {
	engine := createTestCluster()
	if _, err := engine.Schedule(context.Background(), &v1.PlacementRequest{RequestedVRAMMiB: 100}); err != ErrMissingJobID {
		t.Fatalf("want ErrMissingJobID, got %v", err)
	}
}

func fill3050(t *testing.T, engine *PlacementEngine, id string, mib, prio int) *v1.PlacementDecision {
	t.Helper()
	d, err := engine.Schedule(context.Background(), &v1.PlacementRequest{
		JobID: id, WorkloadID: "w", RequestedVRAMMiB: mib, Priority: prio,
		NodeSelector: map[string]string{"cgpu.io/gpu-model": "rtx3050"},
		SubmittedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestReleaseDispatchesAllQueuedJobsThatFit(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	if d := fill3050(t, engine, "big", 3500, 5); d.Status != v1.DecisionPlaced {
		t.Fatalf("big not placed: %+v", d)
	}
	for _, id := range []string{"q1", "q2", "q3"} {
		if d := fill3050(t, engine, id, 1000, 5); d.Status != v1.DecisionPending {
			t.Fatalf("%s should be pending, got %s", id, d.Status)
		}
	}

	reclaimed, dispatched, err := engine.ReleaseJob(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 3500 {
		t.Errorf("reclaimed=%d want 3500", reclaimed)
	}
	if len(dispatched) != 3 {
		t.Fatalf("want all 3 queued jobs dispatched after one release, got %d", len(dispatched))
	}
	if got, ok := engine.GetJob("q3"); !ok || got.Status != v1.DecisionPlaced {
		t.Fatalf("GetJob(q3) = %+v, %v; want PLACED", got, ok)
	}
}

func TestQueuedJobRespectsNodeSelectorOnDispatch(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()

	fill3050(t, engine, "big", 3500, 5)
	if d := fill3050(t, engine, "pinned", 1000, 5); d.Status != v1.DecisionPending {
		t.Fatalf("pinned should queue, got %s", d.Status)
	}
	// Free capacity appears only on the 4050; the pinned job must stay queued.
	if _, err := engine.Schedule(ctx, &v1.PlacementRequest{JobID: "other", RequestedVRAMMiB: 100, SubmittedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.ReleaseJob(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if d, _ := engine.GetJob("pinned"); d.Status != v1.DecisionPending {
		t.Fatalf("pinned job dispatched to wrong GPU: %+v", d)
	}
}

func TestReleaseOfQueuedJobCancelsIt(t *testing.T) {
	engine := createTestCluster()
	fill3050(t, engine, "big", 3500, 5)
	fill3050(t, engine, "waiting", 1000, 5)

	reclaimed, _, err := engine.ReleaseJob(context.Background(), "waiting")
	if err != nil || reclaimed != 0 {
		t.Fatalf("cancel queued: reclaimed=%d err=%v", reclaimed, err)
	}
	if engine.GetPendingQueueLength() != 0 {
		t.Fatal("queued job not removed")
	}
	if _, _, err := engine.ReleaseJob(context.Background(), "never-existed"); err != ErrJobNotFound {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestOfflineNodeGetsNoPlacementsAndQueuesUntilOnline(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()
	for _, n := range engine.GetNodes() {
		engine.SetNodeStatus(n.NodeID, v1.NodeOffline)
	}
	d, err := engine.Schedule(ctx, &v1.PlacementRequest{JobID: "j", RequestedVRAMMiB: 1000, SubmittedAt: time.Now()})
	if err != nil || d.Status != v1.DecisionPending {
		t.Fatalf("with all nodes offline want PENDING, got %+v %v", d, err)
	}
	changed, dispatched := engine.SetNodeStatus("worker-rtx3050", v1.NodeOnline)
	if !changed || len(dispatched) != 1 || dispatched[0].JobID != "j" {
		t.Fatalf("node coming online should dispatch j: changed=%v dispatched=%v", changed, dispatched)
	}
}

func TestReconcileReleasesOnlyStaleOrphans(t *testing.T) {
	engine := createTestCluster()
	ctx := context.Background()
	for _, id := range []string{"alive", "orphan"} {
		if _, err := engine.Schedule(ctx, &v1.PlacementRequest{JobID: id, RequestedVRAMMiB: 500, SubmittedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	live := map[string]bool{"alive": true}

	if rel, _ := engine.ReconcileOrphans(live, time.Minute, time.Now()); len(rel) != 0 {
		t.Fatalf("within grace nothing should be released, got %v", rel)
	}
	rel, _ := engine.ReconcileOrphans(live, time.Minute, time.Now().Add(2*time.Minute))
	if len(rel) != 1 || rel[0] != "orphan" {
		t.Fatalf("want [orphan] released, got %v", rel)
	}
	if _, ok := engine.GetJob("alive"); !ok {
		t.Fatal("live job must keep its reservation")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	path := t.TempDir() + "/state.json"
	ctx := context.Background()

	e1 := createTestCluster()
	if err := e1.EnableStateFile(path); err != nil {
		t.Fatal(err)
	}
	placed := fill3050(t, e1, "big", 3500, 5)
	fill3050(t, e1, "waiting", 1000, 7)

	e2 := createTestCluster()
	if err := e2.EnableStateFile(path); err != nil {
		t.Fatal(err)
	}
	cap, _ := e2.GetPoolCapacity(ctx)
	if cap.AllocatedVRAMMiB != 3500 {
		t.Fatalf("restored allocated=%d want 3500", cap.AllocatedVRAMMiB)
	}
	if d, ok := e2.GetJob("big"); !ok || d.AssignedGPUID != placed.AssignedGPUID {
		t.Fatalf("restored decision mismatch: %+v", d)
	}
	if e2.GetPendingQueueLength() != 1 {
		t.Fatalf("restored queue=%d want 1", e2.GetPendingQueueLength())
	}
	// The restored queue must still dispatch.
	_, dispatched, err := e2.ReleaseJob(ctx, "big")
	if err != nil || len(dispatched) != 1 || dispatched[0].JobID != "waiting" {
		t.Fatalf("after restart release should dispatch waiting: %v %v", dispatched, err)
	}
}

func TestCorruptStateFileRefusesToStart(t *testing.T) {
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createTestCluster().EnableStateFile(path); err == nil {
		t.Fatal("corrupt state file must be an error, not an empty pool")
	}
}

func TestDriverReserveAppliedEvenWhenFreeIsPreset(t *testing.T) {
	// Bootstrap code sets FreeVRAMMiB = TotalVRAMMiB; the reserve must still apply.
	engine := createTestCluster()
	cap, _ := engine.GetPoolCapacity(context.Background())
	if want := (6141 - 512) + (4096 - 512); cap.FreeVRAMMiB != want {
		t.Fatalf("free=%d want %d (total minus 512 MiB reserve per GPU)", cap.FreeVRAMMiB, want)
	}
	d := fill3050(t, engine, "too-big-for-reserve", 3700, 5)
	if d.Status == v1.DecisionPlaced {
		t.Fatal("3700 MiB must not fit a 3584 MiB-allocatable RTX 3050")
	}
}
