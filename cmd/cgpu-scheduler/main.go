package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
	"github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/scheduler"
)

type SchedulerServer struct {
	engine *scheduler.PlacementEngine
}

func main() {
	defaultPort := os.Getenv("PORT")
	if defaultPort == "" {
		defaultPort = "8082"
	}
	port := flag.String("port", defaultPort, "HTTP listen port (env PORT)")
	stateFile := flag.String("state-file", envOr("CGPU_STATE_FILE", "/var/lib/cgpu/scheduler-state.json"),
		"where reservations and the queue are saved so a restart does not lose them; empty disables (env CGPU_STATE_FILE)")
	kubectl := flag.String("kubectl", envOr("CGPU_KUBECTL", "kubectl"), "kubectl binary used by the reconciler (env CGPU_KUBECTL)")
	interval := flag.Duration("reconcile-interval", 30*time.Second, "how often to sync node readiness and release orphaned reservations; 0 disables")
	grace := flag.Duration("orphan-grace", 2*time.Minute, "how long a reservation may exist without a live pod before it is released")
	nodesFile := flag.String("nodes-file", os.Getenv("CGPU_NODES_FILE"),
		"JSON array of GPU nodes (v1.GPUNode) to register instead of the built-in RTX 4050/3050 workers (env CGPU_NODES_FILE)")
	flag.Parse()

	engine := scheduler.NewPlacementEngine()

	// Bootstrap discovered cluster nodes
	if *nodesFile != "" {
		if err := loadNodesFile(engine, *nodesFile); err != nil {
			log.Fatalf("[CGPU-Scheduler] %v", err)
		}
	} else {
		bootstrapClusterNodes(engine)
	}

	if *stateFile != "" {
		if err := engine.EnableStateFile(*stateFile); err != nil {
			log.Fatalf("[CGPU-Scheduler] cannot use state file: %v (fix or move the file; refusing to start with an empty pool while jobs may hold VRAM)", err)
		}
		log.Printf("[CGPU-Scheduler] Persisting scheduler state to %s", *stateFile)
	} else {
		log.Println("[CGPU-Scheduler] WARNING: state persistence disabled; a restart forgets all reservations")
	}

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if *interval > 0 {
		rec := &reconciler{engine: engine, kubectl: *kubectl, interval: *interval, grace: *grace}
		go rec.run(ctx)
		log.Printf("[CGPU-Scheduler] Reconciler enabled (every %s, orphan grace %s)", *interval, *grace)
	}

	server := &SchedulerServer{engine: engine}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/metrics", server.handleMetrics)
	mux.HandleFunc("/api/v1/schedule", server.handleSchedule)
	mux.HandleFunc("/api/v1/release", server.handleRelease)
	mux.HandleFunc("/api/v1/pool", server.handlePoolCapacity)
	mux.HandleFunc("GET /api/v1/jobs/{id}", server.handleGetJob)
	mux.HandleFunc("GET /api/v1/allocations", server.handleAllocations)
	mux.HandleFunc("GET /api/v1/nodes", server.handleNodes)

	httpServer := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[CGPU-Scheduler] Starting placement engine server on port :%s ...", *port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[CGPU-Scheduler] Server failed: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	stop()
	log.Println("[CGPU-Scheduler] Shutting down scheduler service...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("[CGPU-Scheduler] Server shutdown failed: %v", err)
	}
	log.Println("[CGPU-Scheduler] Scheduler exited cleanly.")
}

func loadNodesFile(engine *scheduler.PlacementEngine, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read nodes file: %w", err)
	}
	var nodes []v1.GPUNode
	if err := json.Unmarshal(b, &nodes); err != nil {
		return fmt.Errorf("parse nodes file %s: %w", path, err)
	}
	for i := range nodes {
		if nodes[i].Status == "" {
			nodes[i].Status = v1.NodeOnline
		}
		for g := range nodes[i].GPUs {
			if nodes[i].GPUs[g].HealthStatus == "" {
				nodes[i].GPUs[g].HealthStatus = v1.HealthHealthy
			}
		}
		engine.RegisterOrUpdateNode(&nodes[i])
		log.Printf("[CGPU-Scheduler] Registered node %s from %s", nodes[i].NodeID, path)
	}
	return nil
}

func bootstrapClusterNodes(engine *scheduler.PlacementEngine) {
	// Registered RTX 4050 worker verified in Issue A1/A2
	engine.RegisterOrUpdateNode(&v1.GPUNode{
		NodeID:         "cgpu-gpu-worker-4050",
		KubernetesNode: "cgpu-gpu-worker-4050",
		Hostname:       "cgpu-gpu-worker-4050",
		IPAddress:      "10.20.24.35",
		Status:         v1.NodeOnline,
		Labels: map[string]string{
			"cgpu.io/role":      "gpu-worker",
			"cgpu.io/gpu-model": "rtx4050",
			"cgpu.io/gpu-vram":  "6141MiB",
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
	log.Println("[CGPU-Scheduler] Initialized RTX 4050 GPU worker node (6141 MiB pool).")

	// Registered RTX 3050 worker verified in Issue A4
	engine.RegisterOrUpdateNode(&v1.GPUNode{
		NodeID:         "cgpu-gpu-worker-3050",
		KubernetesNode: "cgpu-gpu-worker-3050",
		Hostname:       "cgpu-gpu-worker-3050",
		IPAddress:      "10.20.24.36",
		Status:         v1.NodeOnline,
		Labels: map[string]string{
			"cgpu.io/role":      "gpu-worker",
			"cgpu.io/gpu-model": "rtx3050",
			"cgpu.io/gpu-vram":  "4096MiB",
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
	log.Println("[CGPU-Scheduler] Initialized RTX 3050 GPU worker node (4096 MiB pool).")
}

func (s *SchedulerServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	cap, err := s.engine.GetPoolCapacity(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get capacity: %v", err), http.StatusInternalServerError)
		return
	}
	nodes := s.engine.GetNodes()
	reserve := s.engine.ContextReserveMiB()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	gauge := func(name, help string) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}

	gauge("cgpu_pool_total_nodes", "Total number of GPU nodes in CGPU pool")
	fmt.Fprintf(w, "cgpu_pool_total_nodes %d\n\n", cap.TotalNodes)
	gauge("cgpu_pool_total_gpus", "Total number of GPUs in CGPU pool")
	fmt.Fprintf(w, "cgpu_pool_total_gpus %d\n\n", cap.TotalGPUs)
	gauge("cgpu_pool_vram_total_mib", "Total physical VRAM across all GPUs in MiB")
	fmt.Fprintf(w, "cgpu_pool_vram_total_mib %d\n\n", cap.AggregateVRAMMiB)
	gauge("cgpu_pool_vram_allocated_mib", "Currently allocated VRAM across all GPUs in MiB")
	fmt.Fprintf(w, "cgpu_pool_vram_allocated_mib %d\n\n", cap.AllocatedVRAMMiB)
	gauge("cgpu_pool_vram_free_mib", "Allocatable VRAM free on online, healthy GPUs in MiB")
	fmt.Fprintf(w, "cgpu_pool_vram_free_mib %d\n\n", cap.FreeVRAMMiB)

	gauge("cgpu_node_online", "1 if the GPU node is Ready in Kubernetes and accepting placements")
	for _, n := range nodes {
		online := 0
		if n.Status == v1.NodeOnline {
			online = 1
		}
		fmt.Fprintf(w, "cgpu_node_online{node=%q} %d\n", n.NodeID, online)
	}
	fmt.Fprintln(w)

	type devMetric struct {
		name, help string
		val        func(g v1.GPUDevice) int
	}
	for _, m := range []devMetric{
		{"cgpu_device_vram_total_mib", "Total physical VRAM per GPU device in MiB", func(g v1.GPUDevice) int { return g.TotalVRAMMiB }},
		{"cgpu_device_vram_allocatable_mib", "Allocatable VRAM per GPU device after driver context overhead in MiB", func(g v1.GPUDevice) int { return g.TotalVRAMMiB - reserve }},
		{"cgpu_device_vram_allocated_mib", "VRAM reserved by active jobs per GPU device in MiB", func(g v1.GPUDevice) int { return g.AllocatedVRAMMiB }},
		{"cgpu_device_active_jobs", "Number of jobs holding a reservation on the GPU device", func(g v1.GPUDevice) int { return len(g.ActiveAllocations) }},
		{"cgpu_device_driver_reserve_mib", "Reserved VRAM for driver context overhead per GPU device in MiB", func(g v1.GPUDevice) int { return reserve }},
	} {
		gauge(m.name, m.help)
		for _, n := range nodes {
			for _, g := range n.GPUs {
				fmt.Fprintf(w, "%s{node=%q,gpu_id=%q,model=%q} %d\n", m.name, n.NodeID, g.GPUID, n.Labels["cgpu.io/gpu-model"], m.val(g))
			}
		}
		fmt.Fprintln(w)
	}

	gauge("cgpu_pending_queue_length", "Number of queued workloads waiting for eligible GPU capacity")
	fmt.Fprintf(w, "cgpu_pending_queue_length %d\n\n", s.engine.GetPendingQueueLength())

	persistOK := 1
	if s.engine.PersistError() != nil {
		persistOK = 0
	}
	gauge("cgpu_state_persist_ok", "1 if the last scheduler state-file write succeeded")
	fmt.Fprintf(w, "cgpu_state_persist_ok %d\n", persistOK)
}

func (s *SchedulerServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "HEALTHY",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *SchedulerServer) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req v1.PlacementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request payload: %v", err), http.StatusBadRequest)
		return
	}

	decision, err := s.engine.Schedule(r.Context(), &req)
	if errors.Is(err, scheduler.ErrMissingJobID) || errors.Is(err, scheduler.ErrInvalidVRAM) {
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("Scheduling error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(decision)
}

type ReleaseRequest struct {
	JobID   string `json:"job_id"`
	NodeID  string `json:"node_id,omitempty"`  // ignored; kept for backward compatibility
	GPUID   string `json:"gpu_id,omitempty"`   // ignored; kept for backward compatibility
	VRAMMiB int    `json:"vram_mib,omitempty"` // ignored; the engine releases what it reserved
}

func (s *SchedulerServer) handleRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid payload: %v", err), http.StatusBadRequest)
		return
	}

	reclaimed, dispatched, err := s.engine.ReleaseJob(r.Context(), req.JobID)
	if errors.Is(err, scheduler.ErrJobNotFound) {
		http.Error(w, fmt.Sprintf("Release failed: %v", err), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("Release failed: %v", err), http.StatusBadRequest)
		return
	}
	logDispatched(dispatched)

	response := map[string]interface{}{
		"status":    "RELEASED",
		"job_id":    req.JobID,
		"reclaimed": reclaimed,
		// Every queued job placed into the freed capacity. The API must create a Kubernetes
		// Job for each (or poll GET /api/v1/jobs/{id}); unclaimed ones are released by the
		// reconciler after its grace period.
		"dispatched_pending_jobs": dispatched,
	}
	if len(dispatched) > 0 {
		response["dispatched_pending_job"] = dispatched[0] // legacy single-job field
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *SchedulerServer) handleGetJob(w http.ResponseWriter, r *http.Request) {
	d, ok := s.engine.GetJob(r.PathValue("id"))
	if !ok {
		http.Error(w, "job not known to scheduler (never submitted, released, or rejected)", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *SchedulerServer) handleAllocations(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"allocations":   s.engine.ListAllocations(),
		"pending_count": s.engine.GetPendingQueueLength(),
	})
}

func (s *SchedulerServer) handleNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.GetNodes())
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *SchedulerServer) handlePoolCapacity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cap, err := s.engine.GetPoolCapacity(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get capacity: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(cap)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
