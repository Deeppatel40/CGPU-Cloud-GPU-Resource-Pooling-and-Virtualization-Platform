package main

import (
	"context"
	"encoding/json"
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	engine := scheduler.NewPlacementEngine()

	// Bootstrap discovered cluster nodes
	bootstrapClusterNodes(engine)

	server := &SchedulerServer{engine: engine}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", server.handleHealthz)
	mux.HandleFunc("/api/v1/schedule", server.handleSchedule)
	mux.HandleFunc("/api/v1/release", server.handleRelease)
	mux.HandleFunc("/api/v1/pool", server.handlePoolCapacity)

	httpServer := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("[CGPU-Scheduler] Starting placement engine server on port :%s ...", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[CGPU-Scheduler] Server failed: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[CGPU-Scheduler] Shutting down scheduler service...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Fatalf("[CGPU-Scheduler] Server shutdown failed: %v", err)
	}
	log.Println("[CGPU-Scheduler] Scheduler exited cleanly.")
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
	if err != nil {
		http.Error(w, fmt.Sprintf("Scheduling error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(decision)
}

type ReleaseRequest struct {
	JobID    string `json:"job_id"`
	NodeID   string `json:"node_id,omitempty"`
	GPUID    string `json:"gpu_id,omitempty"`
	VRAMMiB  int    `json:"vram_mib,omitempty"`
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

	err := s.engine.Release(r.Context(), req.JobID, req.NodeID, req.GPUID, req.VRAMMiB)
	if err != nil {
		http.Error(w, fmt.Sprintf("Release failed: %v", err), http.StatusBadRequest)
		return
	}

	// Trigger check on waiting pending jobs
	poppedReq, poppedDecision, _ := s.engine.PopNextPending(r.Context())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"status":   "RELEASED",
		"job_id":   req.JobID,
		"reclaimed": req.VRAMMiB,
	}
	if poppedReq != nil {
		response["dispatched_pending_job"] = poppedDecision
	}
	_ = json.NewEncoder(w).Encode(response)
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


