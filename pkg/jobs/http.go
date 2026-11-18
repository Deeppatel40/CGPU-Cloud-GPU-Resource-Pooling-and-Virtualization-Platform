package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// Handler returns the public API (/api/v1, see docs/api/job-api-specification.md) plus the
// internal upload endpoint used by workload containers.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "HEALTHY", "time": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /api/v1/workloads", m.handleWorkloads)
	mux.HandleFunc("POST /api/v1/jobs", m.handleSubmit)
	mux.HandleFunc("GET /api/v1/jobs", m.handleList)
	mux.HandleFunc("GET /api/v1/jobs/{id}", m.handleGet)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", m.handleCancel)
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs", m.handleLogs)
	mux.HandleFunc("GET /api/v1/jobs/{id}/artifacts", m.handleArtifacts)
	mux.HandleFunc("GET /api/v1/jobs/{id}/artifacts/{aid}/download", m.handleDownload)
	mux.HandleFunc("GET /api/v1/gpus", m.handleGPUs)
	mux.HandleFunc("GET /api/v1/gpus/{gpu_id}", m.handleGPU)
	mux.HandleFunc("PUT /internal/v1/jobs/{id}/artifacts", m.handleUpload)
	return logRequests(mux)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *APIError) { writeJSON(w, e.Status, e.Body) }

func (m *Manager) handleWorkloads(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{"items": SortedWorkloads(m.Catalog)})
}

func (m *Manager) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req v1.JobSubmissionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		writeErr(w, apiErr(400, v1.ErrCodeValidationFailed, "invalid JSON body: "+err.Error()))
		return
	}
	if req.RequestID == "" {
		req.RequestID = r.Header.Get("X-Request-Id")
	}
	j, created, aerr := m.Submit(r.Context(), req)
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, code, m.Enrich(r.Context(), j))
}

func (m *Manager) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := ListFilter{Status: v1.JobState(strings.ToUpper(q.Get("status"))), WorkloadID: q.Get("workload_id"), Page: 1, Limit: 20}
	switch f.Status {
	case "", v1.StatePending, v1.StateRunning, v1.StateSucceeded, v1.StateFailed:
	default:
		writeErr(w, apiErr(400, v1.ErrCodeValidationFailed, "status must be PENDING, RUNNING, SUCCEEDED or FAILED"))
		return
	}
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		f.Page = p
	}
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 {
		f.Limit = min(l, 100)
	}
	items, total, err := m.Store.List(r.Context(), f)
	if err != nil {
		writeErr(w, apiErr(500, v1.ErrCodeInternalServerError, err.Error()))
		return
	}
	resp := v1.JobListResponse{Items: make([]v1.JobSummary, 0, len(items)), Pagination: v1.PaginationMetadata{
		CurrentPage: f.Page, PageSize: f.Limit, TotalItems: total, TotalPages: int(math.Ceil(float64(total) / float64(f.Limit))),
	}}
	for _, j := range items {
		resp.Items = append(resp.Items, j.Summary())
	}
	writeJSON(w, 200, resp)
}

func (m *Manager) handleGet(w http.ResponseWriter, r *http.Request) {
	j, aerr := m.Get(r.Context(), r.PathValue("id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	writeJSON(w, 200, m.Enrich(r.Context(), j))
}

func (m *Manager) handleCancel(w http.ResponseWriter, r *http.Request) {
	j, aerr := m.Cancel(r.Context(), r.PathValue("id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	writeJSON(w, 200, j.Response())
}

func (m *Manager) handleLogs(w http.ResponseWriter, r *http.Request) {
	j, aerr := m.Get(r.Context(), r.PathValue("id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	tail := 100
	if t, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && t >= 0 {
		tail = min(t, 10000)
	}
	if r.URL.Query().Get("stream") == "true" {
		m.streamLogs(w, r, j, tail)
		return
	}
	lines, err := m.Logs(r.Context(), j, tail)
	if err != nil {
		writeErr(w, apiErr(502, v1.ErrCodeInternalServerError, "could not read logs: "+err.Error()))
		return
	}
	writeJSON(w, 200, LogsResponse{JobID: j.ID, Lines: lines})
}

// streamLogs serves Server-Sent Events: one `data: <LogLine JSON>` per line, then
// `event: end` once the job has finished (or immediately for finished jobs).
func (m *Manager) streamLogs(w http.ResponseWriter, r *http.Request, j *Job, tail int) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, apiErr(500, v1.ErrCodeInternalServerError, "streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	emit := func(l LogLine) error {
		b, _ := json.Marshal(l)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	end := func() {
		fmt.Fprint(w, "event: end\ndata: {}\n\n")
		fl.Flush()
	}
	ctx := r.Context()

	// Wait (with keep-alives) for the pod to start if the job is still being placed.
	for {
		if j.Terminal() {
			lines, _ := m.archivedLogs(j, tail)
			for _, l := range lines {
				if emit(l) != nil {
					return
				}
			}
			end()
			return
		}
		pod, _ := m.LivePod(ctx, j)
		if pod != "" && j.Status == v1.StateRunning {
			if err := m.Kube.StreamLogs(ctx, pod, tail, emit); err != nil && ctx.Err() == nil {
				log.Printf("[CGPU-API] job %s: log stream ended: %v", j.ID, err)
			}
			// The container exited; wait for the reconciler to record the outcome.
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) && ctx.Err() == nil {
				if cur, err := m.Store.Get(ctx, j.ID); err == nil && cur.Terminal() {
					break
				}
				time.Sleep(time.Second)
			}
			end()
			return
		}
		if _, err := fmt.Fprint(w, ": waiting for the workload to start\n\n"); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		cur, err := m.Store.Get(ctx, j.ID)
		if err != nil {
			return
		}
		j = cur
	}
}

func (m *Manager) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	j, aerr := m.Get(r.Context(), r.PathValue("id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	writeJSON(w, 200, map[string]interface{}{"job_id": j.ID, "artifacts": j.Response().Artifacts})
}

func (m *Manager) handleDownload(w http.ResponseWriter, r *http.Request) {
	j, aerr := m.Get(r.Context(), r.PathValue("id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	aid := r.PathValue("aid")
	for _, a := range j.Artifacts {
		if a.ArtifactID != aid {
			continue
		}
		p, err := m.Artifacts.Path(j.ID, a.Name)
		if err != nil {
			break
		}
		f, err := os.Open(p)
		if err != nil {
			writeErr(w, apiErr(404, v1.ErrCodeArtifactNotFound, "artifact file missing on disk"))
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, path.Base(a.Name)))
		w.Header().Set("X-Checksum-SHA256", a.ChecksumSHA256)
		http.ServeContent(w, r, path.Base(a.Name), a.CreatedAt, f)
		return
	}
	writeErr(w, apiErr(404, v1.ErrCodeArtifactNotFound, "artifact "+aid+" not found for job "+j.ID))
}

func (m *Manager) handleGPUs(w http.ResponseWriter, r *http.Request) {
	inv, err := m.Inventory(r.Context())
	if err != nil {
		writeErr(w, apiErr(502, v1.ErrCodeInternalServerError, "scheduler unavailable: "+err.Error()))
		return
	}
	writeJSON(w, 200, inv)
}

func (m *Manager) handleGPU(w http.ResponseWriter, r *http.Request) {
	g, aerr := m.GPU(r.Context(), r.PathValue("gpu_id"))
	if aerr != nil {
		writeErr(w, aerr)
		return
	}
	writeJSON(w, 200, g)
}

// handleUpload receives one output file from a workload container:
// PUT /internal/v1/jobs/{id}/artifacts?path=checkpoints/model_final.pt with
// "Authorization: Bearer <per-job token>".
func (m *Manager) handleUpload(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	meta, err := m.UploadArtifact(r.Context(), r.PathValue("id"), token, r.URL.Query().Get("path"), r.Body)
	switch {
	case err == nil:
		writeJSON(w, 201, meta)
	case errors.Is(err, ErrNotFound):
		writeErr(w, apiErr(404, v1.ErrCodeJobNotFound, "job not found"))
	case errors.Is(err, ErrUnauthorized):
		writeErr(w, apiErr(401, "UNAUTHORIZED", "invalid upload token"))
	case errors.Is(err, ErrBadArtifactPath):
		writeErr(w, apiErr(400, v1.ErrCodeValidationFailed, "invalid artifact path"))
	default:
		writeErr(w, apiErr(409, "UPLOAD_REJECTED", err.Error()))
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" && !(r.Method == http.MethodGet && rec.code < 400) {
			log.Printf("[CGPU-API] %s %s -> %d (%s)", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}
