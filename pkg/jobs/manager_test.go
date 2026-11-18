package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// ---- fakes ----------------------------------------------------------------

type fakeSched struct {
	mu        sync.Mutex
	free      int // VRAM free on the single fake GPU
	placed    map[string]int
	released  []string
	down      bool
	scheduled int
}

func newFakeSched(free int) *fakeSched { return &fakeSched{free: free, placed: map[string]int{}} }

func (s *fakeSched) Schedule(_ context.Context, r *v1.PlacementRequest) (*v1.PlacementDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("connection refused")
	}
	s.scheduled++
	if v, ok := s.placed[r.JobID]; ok {
		return placedDecision(r.JobID, v), nil
	}
	if r.RequestedVRAMMiB > s.free {
		return &v1.PlacementDecision{JobID: r.JobID, Status: v1.DecisionPending, PendingReason: v1.ReasonWaitingForCapacity}, nil
	}
	s.free -= r.RequestedVRAMMiB
	s.placed[r.JobID] = r.RequestedVRAMMiB
	return placedDecision(r.JobID, r.RequestedVRAMMiB), nil
}

func placedDecision(id string, v int) *v1.PlacementDecision {
	return &v1.PlacementDecision{JobID: id, Status: v1.DecisionPlaced, AssignedNode: "cgpu-gpu-worker-4050",
		AssignedGPUID: "GPU-test", Quota: &v1.GPUQuota{AllocatedVRAMMiB: v, EnforcementMode: "CUDA_INTERCEPTOR"},
		PlacementReason: v1.PlacementBestFitFound, DecisionTimestamp: time.Now()}
}

func (s *fakeSched) Release(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.free += s.placed[id]
	delete(s.placed, id)
	s.released = append(s.released, id)
	return nil
}

func (s *fakeSched) Nodes(context.Context) ([]v1.GPUNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []v1.GPUNode{{NodeID: "cgpu-gpu-worker-4050", KubernetesNode: "cgpu-gpu-worker-4050", Status: v1.NodeOnline,
		GPUs: []v1.GPUDevice{{GPUID: "GPU-test", TotalVRAMMiB: 6141, FreeVRAMMiB: s.free, HealthStatus: v1.HealthHealthy}}}}, nil
}
func (s *fakeSched) Pool(context.Context) (*v1.PoolCapacitySummary, error) {
	return &v1.PoolCapacitySummary{}, nil
}
func (s *fakeSched) ContextReserveMiB() int { return 512 }

type fakeKube struct {
	mu        sync.Mutex
	created   map[string][]byte
	states    map[string]*KubeState
	logs      map[string][]LogLine
	deleted   []string
	createErr error
}

func newFakeKube() *fakeKube {
	return &fakeKube{created: map[string][]byte{}, states: map[string]*KubeState{}, logs: map[string][]LogLine{}}
}

func (k *fakeKube) Create(_ context.Context, manifest []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.createErr != nil {
		return k.createErr
	}
	i := bytes.Index(manifest, []byte(`cgpu.io/job-id: "`))
	id := string(manifest[i+len(`cgpu.io/job-id: "`):])
	id = id[:strings.IndexByte(id, '"')]
	k.created[id] = manifest
	k.states[id] = &KubeState{JobExists: true}
	return nil
}

func (k *fakeKube) State(_ context.Context, id string) (*KubeState, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if s, ok := k.states[id]; ok {
		c := *s
		return &c, nil
	}
	return &KubeState{}, nil
}

func (k *fakeKube) setPod(id string, p *PodInfo) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.states[id].Pod = p
}

func (k *fakeKube) Logs(_ context.Context, pod string, _ int) ([]LogLine, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.logs[pod], nil
}
func (k *fakeKube) StreamLogs(context.Context, string, int, func(LogLine) error) error { return nil }
func (k *fakeKube) Delete(_ context.Context, id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.deleted = append(k.deleted, id)
	delete(k.states, id)
	return nil
}

func newTestManager(t *testing.T, free int) (*Manager, *fakeSched, *fakeKube) {
	t.Helper()
	tmpl, err := LoadJobTemplate("../../deploy/templates/cgpu-workload-template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, k := newFakeSched(free), newFakeKube()
	return &Manager{
		Store: NewMemoryStore(), Kube: k, Sched: s, Agents: NewAgentClient(1),
		Artifacts: &ArtifactStore{Root: t.TempDir()}, Catalog: DefaultCatalog(true), Template: tmpl,
		Cfg: Config{Namespace: "cgpu-jobs", APIURL: "http://api:8080", ImagePullGrace: time.Minute},
	}, s, k
}

func submission(rq string, vram int) v1.JobSubmissionRequest {
	return v1.JobSubmissionRequest{RequestID: rq, Name: "run-1", WorkloadID: "pytorch-resnet18-cifar10",
		RequestedVRAMMiB: vram, Parameters: map[string]interface{}{"epochs": float64(2)}}
}

const rq1 = "7b3f9408-e4b2-4d56-b072-20c2420a8511"

// ---- tests ----------------------------------------------------------------

func TestSubmitPlacesAndLaunchesFromSharedTemplate(t *testing.T) {
	m, _, k := newTestManager(t, 5629)
	j, created, aerr := m.Submit(context.Background(), submission(rq1, 2048))
	if aerr != nil || !created {
		t.Fatalf("submit: created=%v err=%v", created, aerr)
	}
	if j.Placement == nil || j.Placement.NodeID != "cgpu-gpu-worker-4050" || j.KubeJobName != KubeName(j.ID) {
		t.Fatalf("job not placed+launched: %+v", j)
	}
	if j.PendingReason != v1.ReasonContainerCreating {
		t.Errorf("pending reason %s", j.PendingReason)
	}
	man := string(k.created[j.ID])
	for _, want := range []string{
		"nodeName: cgpu-gpu-worker-4050", "runtimeClassName: nvidia", "/opt/cgpu/libcgpu_guard.so",
		`name: CGPU_ALLOCATED_VRAM_MIB` + "\n              value: \"2048\"", "activeDeadlineSeconds: 3600",
		`"epochs\":2`, "automountServiceAccountToken: false",
	} {
		if !strings.Contains(man, want) {
			t.Errorf("manifest missing %q", want)
		}
	}
	if !strings.Contains(man, "name: PYTORCH_CUDA_ALLOC_CONF\n              value: \"backend:native\"") || strings.Contains(man, "expandable_segments:True") {
		t.Error("manifest must pin PYTORCH_CUDA_ALLOC_CONF=backend:native (expandable_segments bypasses the guard)")
	}
}

func TestSmokeWorkloadHasNoGPURuntime(t *testing.T) {
	m, _, k := newTestManager(t, 5629)
	r := submission(rq1, 128)
	r.WorkloadID, r.Parameters = "cgpu-pipeline-smoke", nil
	j, _, aerr := m.Submit(context.Background(), r)
	if aerr != nil {
		t.Fatal(aerr)
	}
	man := string(k.created[j.ID])
	if strings.Contains(man, "runtimeClassName: nvidia") || strings.Contains(man, "name: LD_PRELOAD") {
		t.Fatal("CPU smoke workload must not request the nvidia runtime or the guard")
	}
}

func TestIdempotentRetryAndConcurrentDistinct(t *testing.T) {
	m, s, _ := newTestManager(t, 100000)
	ctx := context.Background()
	a, created, _ := m.Submit(ctx, submission(rq1, 1024))
	b, created2, _ := m.Submit(ctx, submission(rq1, 1024))
	if !created || created2 || a.ID != b.ID {
		t.Fatalf("retry must return the same job with created=false: %v %v %s %s", created, created2, a.ID, b.ID)
	}
	if len(s.placed) != 1 {
		t.Fatalf("retry reserved VRAM twice: %v", s.placed)
	}

	// Same request_id racing: exactly one job.
	var wg sync.WaitGroup
	ids := make([]string, 10)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, _, _ := m.Submit(ctx, submission("11111111-2222-4333-8444-555555555555", 1024))
			ids[i] = j.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent same request_id produced different jobs: %v", ids)
		}
	}

	// Distinct request_ids racing: distinct jobs.
	seen := sync.Map{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, _, aerr := m.Submit(ctx, submission(fmt.Sprintf("aaaaaaaa-bbbb-4ccc-8ddd-%012d", i), 1024))
			if aerr != nil {
				t.Error(aerr)
				return
			}
			if _, dup := seen.LoadOrStore(j.ID, true); dup {
				t.Errorf("duplicate job id %s", j.ID)
			}
		}(i)
	}
	wg.Wait()
}

func TestValidationReportsEveryField(t *testing.T) {
	m, _, _ := newTestManager(t, 5629)
	_, _, aerr := m.Submit(context.Background(), v1.JobSubmissionRequest{
		RequestID: "nope", Name: "bad name!", WorkloadID: "pytorch-resnet18-cifar10", RequestedVRAMMiB: 9000,
		Priority: 11, TimeoutSeconds: 5, Parameters: map[string]interface{}{"epochs": 2.5, "colour": "red"},
	})
	if aerr == nil || aerr.Status != 400 {
		t.Fatalf("want 400, got %v", aerr)
	}
	fields := map[string]bool{}
	for _, d := range aerr.Body.Details {
		fields[d.Field] = true
	}
	for _, f := range []string{"request_id", "name", "requested_vram_mib", "priority", "timeout_seconds", "parameters.epochs", "parameters.colour"} {
		if !fields[f] {
			t.Errorf("missing validation detail for %s (got %v)", f, fields)
		}
	}
	if _, _, aerr := m.Submit(context.Background(), submission(rq1, 0)); aerr == nil || aerr.Status != 400 {
		t.Error("requested_vram_mib 0 must be rejected")
	}
}

func TestQueuedJobLaunchesWhenCapacityFrees(t *testing.T) {
	m, s, k := newTestManager(t, 1000)
	ctx := context.Background()
	j, _, _ := m.Submit(ctx, submission(rq1, 2048))
	if j.Placement != nil || j.PendingReason != v1.ReasonWaitingForCapacity {
		t.Fatalf("want queued, got %+v", j)
	}
	if d := m.Enrich(ctx, j).PendingDetails; d == nil || !strings.Contains(d.Message, "largest free slice is 1000") {
		t.Fatalf("pending details: %+v", d)
	}
	s.mu.Lock()
	s.free = 5000
	s.mu.Unlock()
	m.ReconcileAll(ctx)
	j, _ = m.Store.Get(ctx, j.ID)
	if j.Placement == nil || k.created[j.ID] == nil {
		t.Fatalf("queued job not launched after capacity freed: %+v", j)
	}
}

func TestSchedulerOutageKeepsJobPendingThenRecovers(t *testing.T) {
	m, s, _ := newTestManager(t, 5000)
	s.down = true
	ctx := context.Background()
	j, created, aerr := m.Submit(ctx, submission(rq1, 1024))
	if aerr != nil || !created || j.PendingReason != v1.ReasonSchedulingInProgress {
		t.Fatalf("outage: %+v %v", j, aerr)
	}
	s.down = false
	m.ReconcileAll(ctx)
	if j, _ = m.Store.Get(ctx, j.ID); j.Placement == nil {
		t.Fatal("job not placed after scheduler came back")
	}
}

func TestLaunchFailureRollsBackReservation(t *testing.T) {
	m, s, k := newTestManager(t, 5000)
	k.createErr = errors.New("admission webhook denied")
	j, _, _ := m.Submit(context.Background(), submission(rq1, 1024))
	if j.Status != v1.StateFailed || j.FailureReason != v1.ReasonNodeFailure {
		t.Fatalf("want FAILED/NODE_FAILURE, got %s/%s", j.Status, j.FailureReason)
	}
	if len(s.released) != 1 || s.free != 5000 {
		t.Fatalf("reservation not rolled back: released=%v free=%d", s.released, s.free)
	}
}

func runToPod(t *testing.T, m *Manager, k *fakeKube) *Job {
	t.Helper()
	j, _, aerr := m.Submit(context.Background(), submission(rq1, 1024))
	if aerr != nil {
		t.Fatal(aerr)
	}
	start := time.Now().Add(-time.Minute)
	k.setPod(j.ID, &PodInfo{Name: "pod-1", Phase: "Running", Node: "cgpu-gpu-worker-4050", StartedAt: &start})
	m.ReconcileAll(context.Background())
	j, _ = m.Store.Get(context.Background(), j.ID)
	if j.Status != v1.StateRunning || j.Timestamps.StartedAt == nil {
		t.Fatalf("want RUNNING, got %+v", j)
	}
	return j
}

func TestSuccessArchivesLogsReleasesAndCleansUp(t *testing.T) {
	m, s, k := newTestManager(t, 5000)
	ctx := context.Background()
	j := runToPod(t, m, k)

	// Workload uploads its outputs before exiting.
	if _, err := m.UploadArtifact(ctx, j.ID, j.UploadToken, "checkpoints/model_final.pt", strings.NewReader("weights")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UploadArtifact(ctx, j.ID, j.UploadToken, "metrics.json", strings.NewReader(`{"final_accuracy": 61.5, "epochs_completed": 2}`)); err != nil {
		t.Fatal(err)
	}
	k.logs["pod-1"] = []LogLine{{Timestamp: time.Now(), Stream: "stdout", Message: "Epoch [2/2] loss 1.1"}}
	k.setPod(j.ID, &PodInfo{Name: "pod-1", Phase: "Succeeded", Terminated: &Termination{ExitCode: 0, Reason: "Completed", FinishedAt: time.Now()}})
	m.ReconcileAll(ctx)

	j, _ = m.Store.Get(ctx, j.ID)
	if j.Status != v1.StateSucceeded || j.Timestamps.CompletedAt == nil {
		t.Fatalf("want SUCCEEDED, got %s", j.Status)
	}
	if j.ExecutionMetrics == nil || *j.ExecutionMetrics.FinalAccuracy != 61.5 {
		t.Errorf("metrics not parsed: %+v", j.ExecutionMetrics)
	}
	types := map[string]bool{}
	for _, a := range j.Artifacts {
		types[a.ArtifactType] = true
	}
	if !types["model_weights"] || !types["metrics"] || !types["logs"] {
		t.Errorf("artifacts: %+v", j.Artifacts)
	}
	if len(s.released) != 1 || len(k.deleted) != 1 || !j.ReservationReleased {
		t.Errorf("release=%v deleted=%v", s.released, k.deleted)
	}
	logs, _ := m.Logs(ctx, j, 10)
	if len(logs) != 1 || !strings.Contains(logs[0].Message, "Epoch [2/2]") {
		t.Errorf("archived logs: %+v", logs)
	}
	if _, err := m.UploadArtifact(ctx, j.ID, j.UploadToken, "late.pt", strings.NewReader("x")); err == nil {
		t.Error("uploads after completion must be rejected")
	}
}

func TestOOMClassifiedAsCUDAOutOfMemory(t *testing.T) {
	m, _, k := newTestManager(t, 5000)
	j := runToPod(t, m, k)
	k.logs["pod-1"] = []LogLine{
		{Message: "[CGPU-GUARD] cuMemAlloc_v2 1200 MiB deny [CUDA_ERROR_OUT_OF_MEMORY]"},
		{Message: "torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 1.17 GiB"},
	}
	k.setPod(j.ID, &PodInfo{Name: "pod-1", Phase: "Failed", Terminated: &Termination{ExitCode: 1, Reason: "Error", FinishedAt: time.Now()}})
	m.ReconcileAll(context.Background())
	j, _ = m.Store.Get(context.Background(), j.ID)
	if j.Status != v1.StateFailed || j.FailureReason != v1.ReasonCUDAOutOfMemory || j.FailureDetails.ExitCode != 1 {
		t.Fatalf("want FAILED/CUDA_OUT_OF_MEMORY exit 1, got %s/%s %+v", j.Status, j.FailureReason, j.FailureDetails)
	}
}

func TestNonZeroExitAndDeadline(t *testing.T) {
	m, _, k := newTestManager(t, 5000)
	j := runToPod(t, m, k)
	k.mu.Lock()
	k.states[j.ID].JobFailed = true
	k.states[j.ID].JobFailReason = "DeadlineExceeded: Job was active longer than specified deadline"
	k.mu.Unlock()
	m.ReconcileAll(context.Background())
	j, _ = m.Store.Get(context.Background(), j.ID)
	if j.Status != v1.StateFailed || !strings.Contains(j.FailureDetails.ErrorMessage, "DeadlineExceeded") {
		t.Fatalf("deadline: %+v", j.FailureDetails)
	}
}

func TestImageNeverPullFailsFast(t *testing.T) {
	m, s, k := newTestManager(t, 5000)
	j, _, _ := m.Submit(context.Background(), submission(rq1, 1024))
	k.setPod(j.ID, &PodInfo{Name: "pod-1", Phase: "Pending", WaitingReason: "ErrImageNeverPull", CreatedAt: time.Now()})
	m.ReconcileAll(context.Background())
	j, _ = m.Store.Get(context.Background(), j.ID)
	if j.Status != v1.StateFailed || len(s.released) != 1 {
		t.Fatalf("image failure: %s released=%v", j.Status, s.released)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	m, s, k := newTestManager(t, 5000)
	ctx := context.Background()
	j := runToPod(t, m, k)
	c, aerr := m.Cancel(ctx, j.ID)
	if aerr != nil || c.FailureReason != v1.ReasonCancelledByUser || len(k.deleted) != 1 || len(s.released) != 1 {
		t.Fatalf("cancel running: %+v %v", c, aerr)
	}
	if _, aerr := m.Cancel(ctx, j.ID); aerr == nil || aerr.Status != 409 {
		t.Fatal("cancelling a finished job must be 409")
	}
	if _, aerr := m.Cancel(ctx, "job-missing"); aerr == nil || aerr.Status != 404 {
		t.Fatal("cancelling a missing job must be 404")
	}
}

func TestUploadAuthAndPathSafety(t *testing.T) {
	m, _, k := newTestManager(t, 5000)
	j := runToPod(t, m, k)
	ctx := context.Background()
	if _, err := m.UploadArtifact(ctx, j.ID, "wrong", "a.pt", strings.NewReader("x")); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong token: %v", err)
	}
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "a/../../b", ".hidden", "logs/workload.log", ""} {
		if _, err := m.UploadArtifact(ctx, j.ID, j.UploadToken, p, strings.NewReader("x")); !errors.Is(err, ErrBadArtifactPath) {
			t.Errorf("path %q accepted: %v", p, err)
		}
	}
}

func TestHTTPContract(t *testing.T) {
	m, _, k := newTestManager(t, 5000)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	post := func(body string) (*http.Response, map[string]interface{}) {
		resp, err := http.Post(srv.URL+"/api/v1/jobs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]interface{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	body := `{"request_id":"req-` + rq1 + `","name":"resnet18-cifar10-run-1","workload_id":"pytorch-resnet18-cifar10","requested_vram_mib":2048,"parameters":{"epochs":2}}`
	r1, j1 := post(body)
	r2, j2 := post(body)
	if r1.StatusCode != 201 || r2.StatusCode != 200 || j1["job_id"] != j2["job_id"] {
		t.Fatalf("idempotency over HTTP: %d %d %v %v", r1.StatusCode, r2.StatusCode, j1["job_id"], j2["job_id"])
	}
	if r, e := post(`{"request_id":"x","name":"a","workload_id":"pytorch-resnet18-cifar10","requested_vram_mib":-1}`); r.StatusCode != 400 || e["error_code"] != "VALIDATION_FAILED" {
		t.Fatalf("validation over HTTP: %d %v", r.StatusCode, e)
	}

	id := j1["job_id"].(string)
	// Upload through the internal endpoint, then download through the public one.
	start := time.Now()
	k.setPod(id, &PodInfo{Name: "pod-1", Phase: "Running", StartedAt: &start})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/internal/v1/jobs/"+id+"/artifacts?path=checkpoints/model_final.pt", strings.NewReader("trained-weights"))
	j, _ := m.Store.Get(context.Background(), id)
	req.Header.Set("Authorization", "Bearer "+j.UploadToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("upload: %v %v", err, resp.StatusCode)
	}
	var meta v1.ArtifactMetadata
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	resp.Body.Close()
	dl, err := http.Get(srv.URL + meta.DownloadURL)
	if err != nil || dl.StatusCode != 200 {
		t.Fatalf("download: %v %d", err, dl.StatusCode)
	}
	got := new(bytes.Buffer)
	_, _ = got.ReadFrom(dl.Body)
	dl.Body.Close()
	if got.String() != "trained-weights" || dl.Header.Get("X-Checksum-SHA256") != meta.ChecksumSHA256 {
		t.Fatalf("downloaded %q", got.String())
	}

	lr, _ := http.Get(srv.URL + "/api/v1/jobs?status=pending&limit=5")
	var list v1.JobListResponse
	_ = json.NewDecoder(lr.Body).Decode(&list)
	lr.Body.Close()
	if lr.StatusCode != 200 || list.Pagination.TotalItems != 1 || list.Pagination.PageSize != 5 {
		t.Fatalf("list: %d %+v", lr.StatusCode, list.Pagination)
	}
	if g, _ := http.Get(srv.URL + "/api/v1/jobs/job-nope"); g.StatusCode != 404 {
		t.Fatalf("missing job: %d", g.StatusCode)
	}
	wl, _ := http.Get(srv.URL + "/api/v1/workloads")
	if wl.StatusCode != 200 {
		t.Fatalf("workloads: %d", wl.StatusCode)
	}
}

func TestMain(m *testing.M) { os.Exit(m.Run()) }
