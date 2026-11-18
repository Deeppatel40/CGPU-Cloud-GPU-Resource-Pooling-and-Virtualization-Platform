package jobs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// Config holds deployment settings for the manager.
type Config struct {
	Namespace string
	// APIURL is the address workload pods use to upload outputs (must be reachable from
	// the GPU workers, e.g. http://10.20.24.32:8080).
	APIURL string
	// ImagePullGrace is how long a pod may sit in ImagePullBackOff before the job fails.
	ImagePullGrace time.Duration
}

// Manager runs the job lifecycle:
//
//	submit -> (scheduler) PLACED -> create K8s Job -> pod Running -> pod finished
//	   |           \-> PENDING (queued; retried every reconcile pass until placed)
//	   \-> 400 on invalid input; 200 with the existing job on a repeated request_id
//
// Every state change is written to the Store, so a restarted API resumes where it left
// off. One mutex per job serialises the reconcile loop, cancellations and uploads; run
// a single API instance.
type Manager struct {
	Store     Store
	Kube      Kube
	Sched     Scheduler
	Agents    *AgentClient
	Artifacts *ArtifactStore
	Catalog   map[string]*Workload
	Template  *template.Template
	Cfg       Config
	Now       func() time.Time

	locks sync.Map
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) lock(id string) func() {
	v, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// APIError is a failure with an HTTP status and the contract's ErrorResponse body.
type APIError struct {
	Status int
	Body   *v1.ErrorResponse
}

func (e *APIError) Error() string { return e.Body.Message }

func apiErr(status int, code, msg string, details ...v1.ValidationErrorDetail) *APIError {
	return &APIError{Status: status, Body: &v1.ErrorResponse{ErrorCode: code, Message: msg, Details: details, Timestamp: time.Now().UTC()}}
}

var (
	nameRE      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	uuidRE      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	maxTimeoutS = 7 * 24 * 3600
)

// validate checks a submission, fills defaults and returns field-level errors.
func (m *Manager) validate(ctx context.Context, r *v1.JobSubmissionRequest) (*Workload, map[string]interface{}, []v1.ValidationErrorDetail) {
	var errs []v1.ValidationErrorDetail
	add := func(field string, val interface{}, msg string) {
		errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val, Message: msg})
	}
	if !uuidRE.MatchString(strings.TrimPrefix(r.RequestID, "req-")) {
		add("request_id", r.RequestID, "must be a UUIDv4 (optionally prefixed with req-)")
	}
	if !nameRE.MatchString(r.Name) {
		add("name", r.Name, "must be 1-64 characters: letters, digits, '-' or '_'")
	}
	w := m.Catalog[r.WorkloadID]
	if w == nil {
		add("workload_id", r.WorkloadID, "is not an approved workload (see GET /api/v1/workloads)")
	}
	if r.RequestedVRAMMiB <= 0 {
		add("requested_vram_mib", r.RequestedVRAMMiB, "must be a positive integer (MiB)")
	} else if w != nil && (r.RequestedVRAMMiB < w.MinVRAMMiB || r.RequestedVRAMMiB > w.MaxVRAMMiB) {
		add("requested_vram_mib", r.RequestedVRAMMiB, fmt.Sprintf("must be between %d and %d MiB for %s", w.MinVRAMMiB, w.MaxVRAMMiB, w.WorkloadID))
	} else if maxGPU := m.largestAllocatable(ctx); maxGPU > 0 && r.RequestedVRAMMiB > maxGPU {
		add("requested_vram_mib", r.RequestedVRAMMiB, fmt.Sprintf("exceeds the largest GPU slice in the pool (%d MiB after the driver reserve)", maxGPU))
	}
	if r.Priority == 0 {
		r.Priority = 5
	}
	if r.Priority < 1 || r.Priority > 10 {
		add("priority", r.Priority, "must be between 1 and 10")
	}
	if r.TimeoutSeconds == 0 {
		r.TimeoutSeconds = 3600
	}
	if r.TimeoutSeconds < 60 || r.TimeoutSeconds > maxTimeoutS {
		add("timeout_seconds", r.TimeoutSeconds, fmt.Sprintf("must be between 60 and %d", maxTimeoutS))
	}
	if r.Dataset != nil && r.Dataset.Type != v1.DatasetBuiltin {
		add("dataset.type", r.Dataset.Type, "only builtin datasets are supported")
	}
	var params map[string]interface{}
	if w != nil {
		var perrs []v1.ValidationErrorDetail
		params, perrs = w.ResolveParameters(r.Parameters)
		errs = append(errs, perrs...)
	}
	return w, params, errs
}

// largestAllocatable is the biggest single-GPU slice the pool could ever offer (offline
// nodes included: a job may wait for a sleeping laptop). 0 if the scheduler is unreachable.
func (m *Manager) largestAllocatable(ctx context.Context) int {
	nodes, err := m.Sched.Nodes(ctx)
	if err != nil {
		return 0
	}
	best := 0
	for _, n := range nodes {
		for _, g := range n.GPUs {
			if a := g.TotalVRAMMiB - m.Sched.ContextReserveMiB(); a > best {
				best = a
			}
		}
	}
	return best
}

// Submit admits a job. It returns created=false (HTTP 200) when request_id was seen before.
func (m *Manager) Submit(ctx context.Context, r v1.JobSubmissionRequest) (*Job, bool, *APIError) {
	if r.RequestID != "" {
		if existing, err := m.Store.GetByRequestID(ctx, r.RequestID); err == nil {
			return existing, false, nil
		}
	}
	_, params, verrs := m.validate(ctx, &r)
	if len(verrs) > 0 {
		return nil, false, apiErr(400, v1.ErrCodeValidationFailed, "request validation failed", verrs...)
	}

	now := m.now()
	j := &Job{
		ID:               "job-" + newUUID(),
		RequestID:        r.RequestID,
		Name:             r.Name,
		WorkloadID:       r.WorkloadID,
		Status:           v1.StatePending,
		PendingReason:    v1.ReasonSchedulingInProgress,
		RequestedVRAMMiB: r.RequestedVRAMMiB,
		Priority:         r.Priority,
		TimeoutSeconds:   r.TimeoutSeconds,
		Parameters:       params,
		Dataset:          r.Dataset,
		Artifacts:        []v1.ArtifactMetadata{},
		Timestamps:       v1.JobTimestamps{SubmittedAt: now},
		UploadToken:      newToken(),
		UpdatedAt:        now,
	}
	if err := m.Store.Insert(ctx, j); err != nil {
		if errors.Is(err, ErrDuplicateRequest) {
			existing, gerr := m.Store.GetByRequestID(ctx, r.RequestID)
			if gerr == nil {
				return existing, false, nil
			}
		}
		return nil, false, apiErr(500, v1.ErrCodeInternalServerError, "could not store job: "+err.Error())
	}
	log.Printf("[CGPU-API] job %s admitted (%s, %d MiB, request %s)", j.ID, j.WorkloadID, j.RequestedVRAMMiB, j.RequestID)

	// Try to place it right away so the response already shows PLACED or the queue reason.
	unlock := m.lock(j.ID)
	defer unlock()
	if cur, err := m.Store.Get(ctx, j.ID); err == nil {
		j = cur
	}
	if m.step(ctx, j) {
		m.save(ctx, j)
	}
	return j, true, nil
}

func (m *Manager) save(ctx context.Context, j *Job) {
	j.UpdatedAt = m.now()
	if err := m.Store.Replace(ctx, j); err != nil {
		log.Printf("[CGPU-API] job %s: save failed: %v", j.ID, err)
	}
}

// Run reconciles all active jobs every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.ReconcileAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReconcileAll advances every non-terminal job one step.
func (m *Manager) ReconcileAll(ctx context.Context) {
	active, err := m.Store.ListActive(ctx)
	if err != nil {
		log.Printf("[CGPU-API] reconcile: list active jobs: %v", err)
		return
	}
	for _, a := range active {
		unlock := m.lock(a.ID)
		j, err := m.Store.Get(ctx, a.ID)
		if err == nil && !j.Terminal() && m.step(ctx, j) {
			m.save(ctx, j)
		}
		unlock()
	}
}

// step advances one job and reports whether it changed. Caller holds the job lock.
func (m *Manager) step(ctx context.Context, j *Job) bool {
	if j.Terminal() {
		return false
	}
	before := fingerprint(j)
	now := m.now()

	// 1. Waiting for a GPU slice.
	if j.Placement == nil {
		if now.Sub(j.Timestamps.SubmittedAt) > time.Duration(j.TimeoutSeconds)*time.Second {
			m.failBeforeRun(ctx, j, v1.ReasonSchedulingTimeout,
				fmt.Sprintf("not placed on a GPU within timeout_seconds=%d", j.TimeoutSeconds))
			return true
		}
		d, err := m.Sched.Schedule(ctx, &v1.PlacementRequest{
			JobID: j.ID, WorkloadID: j.WorkloadID, RequestedVRAMMiB: j.RequestedVRAMMiB,
			Priority: j.Priority, SubmittedAt: j.Timestamps.SubmittedAt,
		})
		if err != nil {
			j.PendingReason = v1.ReasonSchedulingInProgress
			j.LastError = err.Error()
			return fingerprint(j) != before
		}
		j.LastError = ""
		switch d.Status {
		case v1.DecisionPending:
			j.PendingReason = d.PendingReason
			if j.PendingReason == "" {
				j.PendingReason = v1.ReasonWaitingForCapacity
			}
			return fingerprint(j) != before
		case v1.DecisionRejected:
			reason := v1.ReasonInvalidWorkloadParams
			if d.RejectionReason == v1.RejectionNoEligibleNodes {
				reason = v1.ReasonNodeFailure
			}
			m.failBeforeRun(ctx, j, reason, "scheduler rejected the job: "+string(d.RejectionReason))
			return true
		case v1.DecisionPlaced:
			if d.Quota == nil {
				d.Quota = &v1.GPUQuota{AllocatedVRAMMiB: j.RequestedVRAMMiB, EnforcementMode: "CUDA_INTERCEPTOR"}
			}
			sched := d.DecisionTimestamp.UTC()
			if sched.IsZero() {
				sched = now
			}
			j.Placement = &v1.PlacementDetails{
				NodeID: d.AssignedNode, GPUID: d.AssignedGPUID, AssignedGPUIndex: d.AssignedGPUIndex,
				AllocatedVRAMMiB: d.Quota.AllocatedVRAMMiB, EnforcementMode: d.Quota.EnforcementMode,
				PlacementReason: string(d.PlacementReason), ScheduledAt: sched,
			}
			j.Timestamps.ScheduledAt = &sched
			log.Printf("[CGPU-API] job %s placed on %s (%s), %d MiB", j.ID, d.AssignedNode, d.AssignedGPUID, d.Quota.AllocatedVRAMMiB)
		default:
			j.LastError = "unexpected scheduler decision " + string(d.Status)
			return true
		}
	}

	// 2. Placed but not launched yet (also covers an API crash between the two).
	if j.KubeJobName == "" {
		if err := m.launch(ctx, j); err != nil {
			// Rollback guarantee: a job that cannot be launched gives its VRAM back.
			m.failBeforeRun(ctx, j, v1.ReasonNodeFailure, "could not create the Kubernetes Job: "+err.Error())
			return true
		}
		j.PendingReason = v1.ReasonContainerCreating
		return true
	}

	// 3. Follow the Kubernetes Job.
	st, err := m.Kube.State(ctx, j.ID)
	if err != nil {
		j.LastError = err.Error()
		return fingerprint(j) != before
	}
	j.LastError = ""
	pod := st.Pod
	switch {
	case pod == nil && !st.JobExists:
		if j.Timestamps.ScheduledAt != nil && now.Sub(*j.Timestamps.ScheduledAt) > 30*time.Second {
			m.finish(ctx, j, nil, st, "the Kubernetes Job was deleted outside CGPU")
			return true
		}
	case pod == nil && st.JobFailed:
		m.finish(ctx, j, nil, st, st.JobFailReason)
		return true
	case pod == nil:
		j.PendingReason = v1.ReasonContainerCreating
	case pod.Terminated != nil || pod.Phase == "Succeeded" || pod.Phase == "Failed":
		m.finish(ctx, j, pod, st, "")
		return true
	case st.JobFailed:
		// e.g. activeDeadlineSeconds hit while the pod is still being killed.
		m.finish(ctx, j, pod, st, st.JobFailReason)
		return true
	case pod.Phase == "Running":
		if j.Status != v1.StateRunning {
			log.Printf("[CGPU-API] job %s running on %s", j.ID, pod.Node)
		}
		j.Status = v1.StateRunning
		j.SubStatus = "COMPUTING"
		j.PendingReason = ""
		if pod.StartedAt != nil && j.Timestamps.StartedAt == nil {
			t := pod.StartedAt.UTC()
			j.Timestamps.StartedAt = &t
		}
	default: // Pending / Unknown
		switch pod.WaitingReason {
		case "ErrImagePull", "ImagePullBackOff":
			j.PendingReason = v1.ReasonPullingImage
			if now.Sub(pod.CreatedAt) > m.Cfg.ImagePullGrace {
				m.finish(ctx, j, pod, st, fmt.Sprintf("image could not be pulled on %s: %s", pod.Node, pod.WaitingMessage))
				return true
			}
		case "ErrImageNeverPull", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
			m.finish(ctx, j, pod, st, fmt.Sprintf("container could not start on %s: %s %s", pod.Node, pod.WaitingReason, pod.WaitingMessage))
			return true
		default:
			j.PendingReason = v1.ReasonContainerCreating
		}
	}
	return fingerprint(j) != before
}

func fingerprint(j *Job) string {
	b, _ := json.Marshal(struct {
		S  v1.JobState
		SS string
		P  v1.PendingReason
		E  string
		T  v1.JobTimestamps
		PL *v1.PlacementDetails
		K  string
	}{j.Status, j.SubStatus, j.PendingReason, j.LastError, j.Timestamps, j.Placement, j.KubeJobName})
	return string(b)
}

func (m *Manager) launch(ctx context.Context, j *Job) error {
	w := m.Catalog[j.WorkloadID]
	if w == nil {
		return fmt.Errorf("workload %s no longer in catalog", j.WorkloadID)
	}
	params, err := json.Marshal(j.Parameters)
	if err != nil {
		return err
	}
	manifest, err := RenderJob(m.Template, TemplateData{
		JobID: j.ID, Name: KubeName(j.ID), Namespace: m.Cfg.Namespace, WorkloadID: j.WorkloadID,
		Image: w.Image, CommandShell: shellQuote(w.Command),
		AllocatedVRAMMiB: j.Placement.AllocatedVRAMMiB, AssignedGPUID: j.Placement.GPUID, AssignedNode: j.Placement.NodeID,
		TimeoutSeconds: j.TimeoutSeconds, APIURL: m.Cfg.APIURL, ParamsJSON: string(params),
		UploadToken: j.UploadToken, GPU: w.GPU,
	})
	if err != nil {
		return fmt.Errorf("render job template: %w", err)
	}
	if err := m.Kube.Create(ctx, manifest); err != nil {
		if strings.Contains(err.Error(), "AlreadyExists") {
			j.KubeJobName = KubeName(j.ID)
			return nil
		}
		_ = m.Kube.Delete(ctx, j.ID) // the Secret may have been created before the Job failed
		return err
	}
	j.KubeJobName = KubeName(j.ID)
	log.Printf("[CGPU-API] job %s: created Kubernetes Job %s/%s on %s", j.ID, m.Cfg.Namespace, j.KubeJobName, j.Placement.NodeID)
	return nil
}

// failBeforeRun marks a job FAILED before any workload code ran and gives back its
// reservation or queue slot.
func (m *Manager) failBeforeRun(ctx context.Context, j *Job, reason v1.FailureReason, msg string) {
	now := m.now()
	if j.KubeJobName != "" {
		_ = m.Kube.Delete(ctx, j.ID)
	}
	j.Status = v1.StateFailed
	j.SubStatus = string(reason)
	j.PendingReason = ""
	j.FailureReason = reason
	j.FailureDetails = &v1.FailureDetails{ExitCode: -1, ErrorMessage: msg, TerminatedAt: now}
	j.Timestamps.CompletedAt = &now
	m.release(ctx, j)
	log.Printf("[CGPU-API] job %s failed before running: %s: %s", j.ID, reason, msg)
}

func (m *Manager) release(ctx context.Context, j *Job) {
	if err := m.Sched.Release(ctx, j.ID); err != nil {
		// The scheduler's reconciler frees reservations without a live pod after its grace
		// period, so this is logged rather than retried forever.
		j.LastError = "release reservation: " + err.Error()
		log.Printf("[CGPU-API] job %s: %s", j.ID, j.LastError)
		return
	}
	j.ReservationReleased = true
}

var oomRE = regexp.MustCompile(`(?i)(CUDA out of memory|CUDA_ERROR_OUT_OF_MEMORY|cudaErrorMemoryAllocation|torch\.(cuda\.)?OutOfMemoryError|deny \[CUDA_ERROR_OUT_OF_MEMORY\])`)

// finish records the outcome of a job whose workload ran (or tried to): archives the logs,
// classifies the result, deletes the Kubernetes objects and releases the reservation.
func (m *Manager) finish(ctx context.Context, j *Job, pod *PodInfo, st *KubeState, why string) {
	now := m.now()
	var lines []LogLine
	if pod != nil {
		var err error
		if lines, err = m.Kube.Logs(ctx, pod.Name, 0); err != nil {
			log.Printf("[CGPU-API] job %s: could not fetch logs: %v", j.ID, err)
		} else {
			m.archiveLogs(j, lines)
		}
	}

	exit := -1
	finished := now
	if pod != nil && pod.Terminated != nil {
		exit = pod.Terminated.ExitCode
		if !pod.Terminated.FinishedAt.IsZero() {
			finished = pod.Terminated.FinishedAt.UTC()
		}
		if why == "" && pod.Terminated.Reason != "" && pod.Terminated.Reason != "Completed" && pod.Terminated.Reason != "Error" {
			why = pod.Terminated.Reason + " " + pod.Terminated.Message
		}
	}
	if pod != nil && pod.StartedAt != nil && j.Timestamps.StartedAt == nil {
		t := pod.StartedAt.UTC()
		j.Timestamps.StartedAt = &t
	}
	j.Timestamps.CompletedAt = &finished
	j.PendingReason = ""

	success := pod != nil && exit == 0 && pod.Phase == "Succeeded" && !st.JobFailed && why == ""
	if success {
		j.Status = v1.StateSucceeded
		j.SubStatus = "COMPLETED_SUCCESSFULLY"
		log.Printf("[CGPU-API] job %s succeeded", j.ID)
	} else {
		j.Status = v1.StateFailed
		j.FailureReason = v1.ReasonContainerNonZeroExit
		msg := strings.TrimSpace(why)
		if oom := lastMatching(lines, oomRE); oom != "" {
			j.FailureReason = v1.ReasonCUDAOutOfMemory
			if msg == "" {
				msg = oom
			}
		}
		if pod == nil && msg != "" && strings.Contains(msg, "deleted outside") {
			j.FailureReason = v1.ReasonNodeFailure
		}
		if msg == "" {
			msg = fmt.Sprintf("workload exited with code %d", exit)
		}
		j.SubStatus = string(j.FailureReason)
		j.FailureDetails = &v1.FailureDetails{ExitCode: exit, ErrorMessage: msg, Stacktrace: tailText(lines, 15), TerminatedAt: finished}
		log.Printf("[CGPU-API] job %s failed: %s: %s", j.ID, j.FailureReason, msg)
	}

	if err := m.Kube.Delete(ctx, j.ID); err != nil {
		log.Printf("[CGPU-API] job %s: delete Kubernetes objects: %v", j.ID, err)
	}
	m.release(ctx, j)
}

func lastMatching(lines []LogLine, re *regexp.Regexp) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if re.MatchString(lines[i].Message) {
			return strings.TrimSpace(lines[i].Message)
		}
	}
	return ""
}

func tailText(lines []LogLine, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Message)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

const archivedLogPath = "logs/workload.log"

func (m *Manager) archiveLogs(j *Job, lines []LogLine) {
	var buf bytes.Buffer
	for _, l := range lines {
		fmt.Fprintf(&buf, "%s %s\n", l.Timestamp.Format(time.RFC3339Nano), l.Message)
	}
	meta, err := m.Artifacts.Save(j.ID, archivedLogPath, &buf)
	if err != nil {
		log.Printf("[CGPU-API] job %s: archive logs: %v", j.ID, err)
		return
	}
	upsertArtifact(j, *meta)
}

func upsertArtifact(j *Job, a v1.ArtifactMetadata) {
	for i := range j.Artifacts {
		if j.Artifacts[i].ArtifactID == a.ArtifactID {
			j.Artifacts[i] = a
			return
		}
	}
	j.Artifacts = append(j.Artifacts, a)
}

// Get returns one job.
func (m *Manager) Get(ctx context.Context, id string) (*Job, *APIError) {
	j, err := m.Store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, apiErr(404, v1.ErrCodeJobNotFound, "job "+id+" not found")
	}
	if err != nil {
		return nil, apiErr(500, v1.ErrCodeInternalServerError, err.Error())
	}
	return j, nil
}

// Cancel stops a pending or running job.
func (m *Manager) Cancel(ctx context.Context, id string) (*Job, *APIError) {
	unlock := m.lock(id)
	defer unlock()
	j, aerr := m.Get(ctx, id)
	if aerr != nil {
		return nil, aerr
	}
	if j.Terminal() {
		return nil, apiErr(409, "JOB_ALREADY_FINISHED", fmt.Sprintf("job %s already finished with status %s", id, j.Status))
	}
	now := m.now()
	exit := -1
	if j.KubeJobName != "" {
		if st, err := m.Kube.State(ctx, j.ID); err == nil && st.Pod != nil {
			if lines, err := m.Kube.Logs(ctx, st.Pod.Name, 0); err == nil {
				m.archiveLogs(j, lines)
			}
			if st.Pod.Terminated != nil {
				exit = st.Pod.Terminated.ExitCode
			}
		}
		if err := m.Kube.Delete(ctx, j.ID); err != nil {
			return nil, apiErr(500, v1.ErrCodeInternalServerError, "could not stop the Kubernetes Job: "+err.Error())
		}
	}
	j.Status = v1.StateFailed
	j.SubStatus = string(v1.ReasonCancelledByUser)
	j.PendingReason = ""
	j.FailureReason = v1.ReasonCancelledByUser
	j.FailureDetails = &v1.FailureDetails{ExitCode: exit, ErrorMessage: "cancelled by user", TerminatedAt: now}
	j.Timestamps.CompletedAt = &now
	m.release(ctx, j)
	m.save(ctx, j)
	log.Printf("[CGPU-API] job %s cancelled", j.ID)
	return j, nil
}

// ErrUnauthorized is returned for uploads with a wrong token.
var ErrUnauthorized = errors.New("invalid upload token")

// UploadArtifact stores one output file sent by the workload container.
func (m *Manager) UploadArtifact(ctx context.Context, id, token, rel string, body io.Reader) (*v1.ArtifactMetadata, error) {
	j, err := m.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(j.UploadToken)) != 1 {
		return nil, ErrUnauthorized
	}
	if j.Terminal() {
		return nil, fmt.Errorf("job %s is already %s", id, j.Status)
	}
	if clean, err := CleanArtifactPath(rel); err != nil || strings.HasPrefix(clean, "logs/") {
		return nil, ErrBadArtifactPath
	}
	meta, err := m.Artifacts.Save(id, rel, body) // stream to disk outside the job lock
	if err != nil {
		return nil, err
	}

	unlock := m.lock(id)
	defer unlock()
	j, err = m.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	upsertArtifact(j, *meta)
	if meta.ArtifactType == "metrics" {
		if p, err := m.Artifacts.Path(id, meta.Name); err == nil {
			if b, err := os.ReadFile(p); err == nil {
				var em v1.ExecutionMetrics
				if json.Unmarshal(b, &em) == nil {
					j.ExecutionMetrics = &em
				}
			}
		}
	}
	m.save(ctx, j)
	log.Printf("[CGPU-API] job %s: stored artifact %s (%d bytes, sha256 %s)", id, meta.Name, meta.SizeBytes, meta.ChecksumSHA256[:12])
	return meta, nil
}

// Logs returns the latest log lines: live from the pod while it exists, otherwise from the
// archived copy.
func (m *Manager) Logs(ctx context.Context, j *Job, tail int) ([]LogLine, error) {
	if !j.Terminal() && j.KubeJobName != "" {
		st, err := m.Kube.State(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		if st.Pod == nil || st.Pod.Phase == "Pending" && st.Pod.StartedAt == nil {
			return []LogLine{}, nil
		}
		return m.Kube.Logs(ctx, st.Pod.Name, tail)
	}
	return m.archivedLogs(j, tail)
}

func (m *Manager) archivedLogs(j *Job, tail int) ([]LogLine, error) {
	p, err := m.Artifacts.Path(j.ID, archivedLogPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return []LogLine{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []LogLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, parseLogLine(sc.Text()))
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if lines == nil {
		lines = []LogLine{}
	}
	return lines, sc.Err()
}

// LivePod returns the running pod name for streaming, or "" if logs should come from the archive.
func (m *Manager) LivePod(ctx context.Context, j *Job) (string, error) {
	if j.Terminal() || j.KubeJobName == "" {
		return "", nil
	}
	st, err := m.Kube.State(ctx, j.ID)
	if err != nil || st.Pod == nil {
		return "", err
	}
	return st.Pod.Name, nil
}
