// Command cgpu-worker-agent is CGPU's per-worker GPU discovery and telemetry daemon.
//
// It answers A3 criterion 1 ("discover the GPU and expose worker identity, GPU identity,
// health, and allocatable VRAM") and provides the metrics A4 needs to export.
//
// Design notes worth knowing before changing this:
//
//   - GPU facts come from nvidia-smi rather than NVML via cgo. On the project's WSL workers
//     NVML is available but the cgo build chain is extra weight for no gain: the agent needs
//     a handful of scalar fields, and nvidia-smi's CSV output is stable and already proven on
//     these nodes. It also keeps this a dependency-free, pure-Go, cross-compilable binary.
//
//   - Per-job VRAM usage does NOT come from the driver. On WSL workers the driver reports no
//     per-process VRAM at all (--query-compute-apps used_memory is [N/A], pmon is unusable,
//     accounting mode is Disabled - see docs/gpu-worker-4050.md). The only per-job signal is
//     what the CGPU allocation guard writes to its status files, so the agent reads those.
//     Device-level numbers and per-job numbers therefore come from different sources and will
//     not add up exactly; the gap is CUDA context overhead, which is why -reserve-mib exists.
//
//   - Allocatable VRAM is deliberately total minus a reserve, not total minus used. Each CUDA
//     context costs device memory that no job's quota accounts for; measured on the RTX 4050,
//     two jobs holding 1152 MiB showed 1314 MiB in use. Handing out the full total would let
//     the scheduler overcommit the card and a job would fail at startup instead of being
//     queued. Reservation accounting itself belongs to the scheduler, not here.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GPU is one physical device as this worker sees it.
type GPU struct {
	Index          int     `json:"index"`
	UUID           string  `json:"uuid"`
	Name           string  `json:"name"`
	PCIBus         string  `json:"pci_bus_id"`
	MemoryTotalMiB int64   `json:"memory_total_mib"`
	MemoryUsedMiB  int64   `json:"memory_used_mib"`
	MemoryFreeMiB  int64   `json:"memory_free_mib"`
	AllocatableMiB int64   `json:"allocatable_mib"`
	UtilizationPct int64   `json:"utilization_pct"`
	TemperatureC   int64   `json:"temperature_c"`
	PowerDrawW     float64 `json:"power_draw_w"`
	Healthy        bool    `json:"healthy"`
}

// JobUsage is one job's VRAM accounting, as reported by the CGPU allocation guard.
// This is the only per-job VRAM signal available on WSL workers.
type JobUsage struct {
	JobID       string `json:"job_id"`
	UsedBytes   int64  `json:"used_bytes"`
	PeakBytes   int64  `json:"peak_bytes"`
	QuotaBytes  int64  `json:"quota_bytes"`
	Allocations int64  `json:"allocations"`
	Denials     int64  `json:"denials"`
	Source      string `json:"source"`
}

// Inventory is the full picture this worker publishes.
type Inventory struct {
	NodeName    string     `json:"node_name"`
	Hostname    string     `json:"hostname"`
	AgentVer    string     `json:"agent_version"`
	Healthy     bool       `json:"healthy"`
	Message     string     `json:"message,omitempty"`
	DriverVer   string     `json:"driver_version,omitempty"`
	CUDAVer     string     `json:"cuda_version,omitempty"`
	ReserveMiB  int64      `json:"reserve_mib"`
	GPUs        []GPU      `json:"gpus"`
	Jobs        []JobUsage `json:"jobs"`
	CollectedAt time.Time  `json:"collected_at"`
}

const agentVersion = "0.1.0"

var (
	flagListen     = flag.String("listen", ":9400", "address to serve the HTTP API on")
	flagInterval   = flag.Duration("interval", 10*time.Second, "how often to refresh GPU inventory")
	flagReserveMiB = flag.Int64("reserve-mib", 512, "VRAM held back from allocatable for CUDA context overhead")
	flagStatusDir  = flag.String("guard-status-dir", "/var/run/cgpu/guard", "directory holding CGPU guard status files")
	flagNodeName   = flag.String("node-name", "", "worker identity; defaults to $CGPU_NODE_NAME, then hostname")
	flagSMI        = flag.String("nvidia-smi", "nvidia-smi", "path to nvidia-smi")
	flagOnce       = flag.Bool("once", false, "print inventory as JSON once and exit (no server)")
)

type agent struct {
	mu   sync.RWMutex
	inv  Inventory
	name string
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	host, _ := os.Hostname()
	name := *flagNodeName
	if name == "" {
		name = os.Getenv("CGPU_NODE_NAME")
	}
	if name == "" {
		name = host
	}

	a := &agent{name: name}
	a.refresh()

	if *flagOnce {
		a.mu.RLock()
		defer a.mu.RUnlock()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(a.inv); err != nil {
			log.Fatalf("encode: %v", err)
		}
		if !a.inv.Healthy {
			os.Exit(1)
		}
		return
	}

	go func() {
		t := time.NewTicker(*flagInterval)
		defer t.Stop()
		for range t.C {
			a.refresh()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/inventory", a.handleInventory)
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/metrics", a.handleMetrics)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cgpu-worker-agent: try /inventory, /healthz or /metrics", http.StatusNotFound)
	})

	srv := &http.Server{
		Addr:              *flagListen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("cgpu-worker-agent %s serving on %s as node %q (reserve %d MiB)",
		agentVersion, *flagListen, name, *flagReserveMiB)
	log.Fatal(srv.ListenAndServe())
}

// ---- collection ----------------------------------------------------------------------

func (a *agent) refresh() {
	inv := Inventory{
		NodeName:    a.name,
		AgentVer:    agentVersion,
		ReserveMiB:  *flagReserveMiB,
		CollectedAt: time.Now().UTC(),
	}
	inv.Hostname, _ = os.Hostname()

	gpus, driver, cuda, err := a.queryGPUs()
	if err != nil {
		// Unhealthy is a reportable state, not a crash: the scheduler needs to see that this
		// worker cannot currently be trusted with placements.
		inv.Healthy = false
		inv.Message = fmt.Sprintf("GPU discovery failed: %v", err)
		log.Printf("WARN %s", inv.Message)
	} else {
		inv.GPUs = gpus
		inv.DriverVer = driver
		inv.CUDAVer = cuda
		inv.Healthy = len(gpus) > 0
		if len(gpus) == 0 {
			inv.Message = "no GPUs reported by nvidia-smi"
		}
	}

	inv.Jobs = a.readGuardStatus()

	a.mu.Lock()
	a.inv = inv
	a.mu.Unlock()
}

func (a *agent) queryGPUs() ([]GPU, string, string, error) {
	const fields = "index,uuid,name,pci.bus_id,memory.total,memory.used,memory.free," +
		"utilization.gpu,temperature.gpu,power.draw,driver_version"

	out, err := exec.Command(*flagSMI,
		"--query-gpu="+fields, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", *flagSMI, err)
	}

	var gpus []GPU
	var driver string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitCSV(line)
		if len(f) < 11 {
			log.Printf("WARN unexpected nvidia-smi row (%d fields): %q", len(f), line)
			continue
		}
		total := atoi64(f[4])
		g := GPU{
			Index:          int(atoi64(f[0])),
			UUID:           f[1],
			Name:           f[2],
			PCIBus:         f[3],
			MemoryTotalMiB: total,
			MemoryUsedMiB:  atoi64(f[5]),
			MemoryFreeMiB:  atoi64(f[6]),
			UtilizationPct: atoi64(f[7]),
			TemperatureC:   atoi64(f[8]),
			PowerDrawW:     atof64(f[9]),
		}
		g.AllocatableMiB = total - *flagReserveMiB
		if g.AllocatableMiB < 0 {
			g.AllocatableMiB = 0
		}
		// A device we can enumerate, that reports capacity, is healthy. There is no richer
		// health signal on WSL: ECC and MIG are unavailable (see docs/gpu-worker-4050.md).
		g.Healthy = g.UUID != "" && total > 0
		gpus = append(gpus, g)
		driver = f[10]
	}

	cuda := ""
	if b, err := exec.Command(*flagSMI, "--query-gpu=cuda_version", "--format=csv,noheader").Output(); err == nil {
		cuda = strings.TrimSpace(string(b))
	}
	return gpus, driver, cuda, nil
}

// readGuardStatus collects per-job VRAM accounting written by the CGPU allocation guard
// (CGPU_GUARD_STATUS_FILE). Missing or malformed files are skipped rather than failing the
// whole refresh - a job that exits mid-write must not take the agent's telemetry down.
func (a *agent) readGuardStatus() []JobUsage {
	paths, err := filepath.Glob(filepath.Join(*flagStatusDir, "*"))
	if err != nil || len(paths) == 0 {
		return nil
	}
	var jobs []JobUsage
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		j := JobUsage{JobID: filepath.Base(p), Source: p}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			switch k {
			case "job_id":
				j.JobID = v
			case "used_bytes":
				j.UsedBytes = atoi64(v)
			case "peak_bytes":
				j.PeakBytes = atoi64(v)
			case "quota_bytes":
				j.QuotaBytes = atoi64(v)
			case "allocations":
				j.Allocations = atoi64(v)
			case "denials":
				j.Denials = atoi64(v)
			}
		}
		jobs = append(jobs, j)
	}
	return jobs
}

// ---- handlers ------------------------------------------------------------------------

func (a *agent) handleInventory(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	inv := a.inv
	a.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(inv)
}

func (a *agent) handleHealthz(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	inv := a.inv
	a.mu.RUnlock()
	if !inv.Healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "unhealthy: %s\n", inv.Message)
		return
	}
	fmt.Fprintf(w, "ok: %d GPU(s) on %s\n", len(inv.GPUs), inv.NodeName)
}

// handleMetrics emits Prometheus text format for A4's dashboard.
//
// Device-level memory is measured; per-job memory comes from the guard. They are separate
// metric families on purpose, because attributing device usage to jobs is not possible on
// WSL workers and a dashboard must not imply otherwise.
func (a *agent) handleMetrics(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	inv := a.inv
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("# HELP cgpu_worker_healthy Whether this worker can currently accept GPU placements.")
	p("# TYPE cgpu_worker_healthy gauge")
	p("cgpu_worker_healthy{node=%q} %d", inv.NodeName, b2i(inv.Healthy))

	p("# HELP cgpu_gpu_memory_total_mib Total VRAM on the device.")
	p("# TYPE cgpu_gpu_memory_total_mib gauge")
	p("# HELP cgpu_gpu_memory_allocatable_mib VRAM CGPU may hand out (total minus context reserve).")
	p("# TYPE cgpu_gpu_memory_allocatable_mib gauge")
	p("# HELP cgpu_gpu_memory_used_mib Device-level VRAM in use. Measured; not attributable per job on WSL.")
	p("# TYPE cgpu_gpu_memory_used_mib gauge")
	p("# HELP cgpu_gpu_utilization_percent Device utilisation.")
	p("# TYPE cgpu_gpu_utilization_percent gauge")
	p("# HELP cgpu_gpu_healthy Whether the device enumerates and reports capacity.")
	p("# TYPE cgpu_gpu_healthy gauge")

	for _, g := range inv.GPUs {
		l := fmt.Sprintf("node=%q,gpu=%q,uuid=%q,model=%q", inv.NodeName, strconv.Itoa(g.Index), g.UUID, g.Name)
		p("cgpu_gpu_memory_total_mib{%s} %d", l, g.MemoryTotalMiB)
		p("cgpu_gpu_memory_allocatable_mib{%s} %d", l, g.AllocatableMiB)
		p("cgpu_gpu_memory_used_mib{%s} %d", l, g.MemoryUsedMiB)
		p("cgpu_gpu_utilization_percent{%s} %d", l, g.UtilizationPct)
		p("cgpu_gpu_temperature_celsius{%s} %d", l, g.TemperatureC)
		p("cgpu_gpu_power_draw_watts{%s} %g", l, g.PowerDrawW)
		p("cgpu_gpu_healthy{%s} %d", l, b2i(g.Healthy))
	}

	p("# HELP cgpu_gpu_active_jobs Jobs currently reporting through the CGPU allocation guard.")
	p("# TYPE cgpu_gpu_active_jobs gauge")
	p("cgpu_gpu_active_jobs{node=%q} %d", inv.NodeName, len(inv.Jobs))

	p("# HELP cgpu_job_vram_used_bytes Per-job VRAM held, as tracked by the CGPU guard (not the driver).")
	p("# TYPE cgpu_job_vram_used_bytes gauge")
	p("# HELP cgpu_job_vram_quota_bytes Per-job VRAM quota assigned by the scheduler.")
	p("# TYPE cgpu_job_vram_quota_bytes gauge")
	p("# HELP cgpu_job_vram_denials_total Allocations refused for exceeding the job's quota.")
	p("# TYPE cgpu_job_vram_denials_total counter")
	for _, j := range inv.Jobs {
		l := fmt.Sprintf("node=%q,job_id=%q", inv.NodeName, j.JobID)
		p("cgpu_job_vram_used_bytes{%s} %d", l, j.UsedBytes)
		p("cgpu_job_vram_peak_bytes{%s} %d", l, j.PeakBytes)
		p("cgpu_job_vram_quota_bytes{%s} %d", l, j.QuotaBytes)
		p("cgpu_job_vram_denials_total{%s} %d", l, j.Denials)
	}
}

// ---- helpers -------------------------------------------------------------------------

func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func atoi64(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "[N/A]" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func atof64(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "[N/A]" || s == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
