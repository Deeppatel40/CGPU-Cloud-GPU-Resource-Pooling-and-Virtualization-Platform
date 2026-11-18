# 🎓 CGPU — Complete Project Guide & Presentation Playbook
> **CSE540: Cloud Computing (Group 06 — Ahmedabad University)**  
> **Project:** CGPU (Cloud GPU Resource Pooling and Virtualization Platform)  
> **Team:** Nishit Patel, Deep Patel, Nirav Shah, Subrat Jain  
> **Date:** November 2026

---

## 📑 Table of Contents
1. [The 60-Second Elevator Pitch (What is this project?)](#1-the-60-second-elevator-pitch)
2. [Hardware & Network Topology Map](#2-hardware--network-topology-map)
3. [What Was Built (Component by Component)](#3-what-was-built)
4. [Step-by-Step Professor Demonstration Script](#4-step-by-step-professor-demonstration-script)
5. [Professor Q&A Cheat Sheet (Tough Questions & Exact Answers)](#5-professor-qa-cheat-sheet)
6. [Emergency Troubleshooting Guide (If anything goes wrong in the demo!)](#6-emergency-troubleshooting-guide)
7. [Team Roles & Contributions](#7-team-roles--contributions)
8. [Quick Command Cheat Sheet](#8-quick-command-cheat-sheet)

---

## 1. The 60-Second Elevator Pitch

### What problem are we solving?
In typical cloud environments, if a student or researcher submits a machine learning job requesting **1 GB of VRAM**, the cloud scheduler assigns them an **entire physical GPU** (e.g., 6 GB or 16 GB). The remaining memory is completely wasted, and nobody else can use that GPU until the job finishes. Furthermore, consumer laptops with GPUs (like RTX 4050 / RTX 3050) sitting in universities cannot easily participate in Kubernetes clusters because Windows WSL2 isolates GPU memory and lacks native multi-tenant vGPU hardware support.

### What did our group build?
We built **CGPU**, an end-to-end distributed GPU virtualization and resource pooling platform that:
1. **Aggregates Heterogeneous Consumer GPUs**: Connects physical Ubuntu desktop PCs and Windows 11 laptops (RTX 4050 + RTX 3050) over the campus LAN into a single unified Kubernetes cluster (K3s).
2. **Virtualizes GPU Memory (Fractional VRAM)**: Built our own custom C-based dynamic interceptor (`libcgpu_guard.so`) via `LD_PRELOAD` that hooks CUDA runtime and driver calls (`cudaMalloc`, `cuMemAlloc`, `cudaMemGetInfo`). It restricts workloads to an exact VRAM slice (e.g., 1024 MiB) without needing expensive enterprise NVIDIA vGPU hardware or licenses.
3. **Implements Best-Fit Binpacking Scheduling**: Developed a central Go scheduler daemon that scores GPUs, reserves 512 MiB for CUDA driver context overhead, atomically packs fractional workloads onto the best-fit GPU to minimize memory fragmentation, and manages dynamic queueing.
4. **Full Observability**: Provides Prometheus metrics (`:8082/metrics`) and a live real-time HTML5 cluster status dashboard (`:8081`).

---

## 2. Hardware & Network Topology Map

Our cluster runs across **4 distinct physical machines** on the local subnet (`10.20.24.0/24`):

```
                       +---------------------------------------------+
                       |             LAN SWITCH / ROUTER             |
                       |                10.20.24.0/24                |
                       +---------------------------------------------+
                              |              |              |              |
         +--------------------+              |              |              +--------------------+
         |                                   |              |                                   |
         v                                   v              v                                   v
+-----------------------+  +-----------------------+  +-----------------------+  +-----------------------+
|   Ubuntu PC 1         |  |   Ubuntu PC 2         |  |   Windows 11 Laptop   |  |   Windows 11 Laptop   |
|   10.20.24.31         |  |   10.20.24.32         |  |   10.20.24.35         |  |   10.20.24.36         |
|                       |  |                       |  |                       |  |                       |
|  [cgpu-control-plane] |  |   [cgpu-cpu-worker]   |  | [cgpu-gpu-worker-4050]|  | [cgpu-gpu-worker-3050]|
|  - K3s Control Plane  |  |  - K3s Agent Worker   |  |  - Ubuntu 26.04 WSL2  |  |  - Ubuntu 24.04 WSL2  |
|  - cgpu-scheduler     |  |  - Next.js Web Portal |  |  - NVIDIA RTX 4050    |  |  - NVIDIA RTX 3050    |
|    (:8082 & /metrics) |  |  - Go REST API GW     |  |    (6,141 MiB VRAM)   |  |    (4,096 MiB VRAM)   |
|  - cgpu-dashboard     |  |                       |  |  - Mirrored Network   |  |  - Drive A:\ Storage  |
|    (:8081 Web GUI)    |  |                       |  |  - libcgpu_guard.so   |  |  - libcgpu_guard.so   |
+-----------------------+  +-----------------------+  +-----------------------+  +-----------------------+
```

| Node Name | Hardware & OS | IP Address | Role in CGPU Platform |
| :--- | :--- | :--- | :--- |
| **`cgpu-control-plane`** | Ubuntu Desktop PC 1 (16GB RAM, CPU-only) | `10.20.24.31` | **Kubernetes Master & Control Plane**. Hosts K3s server, `cgpu-scheduler` daemon (:8082), Prometheus telemetry, and `cgpu-dashboard` (:8081). |
| **`cgpu-cpu-worker`** | Ubuntu Desktop PC 2 (16GB RAM, CPU-only) | `10.20.24.32` | **Application & Gateway Worker**. Hosts non-GPU workloads, Go REST API Gateway, and Next.js portal UI. |
| **`cgpu-gpu-worker-4050`** | Deep's Windows 11 Laptop (Ubuntu 26.04 WSL2) | `10.20.24.35` | **GPU Worker 1**. NVIDIA GeForce RTX 4050 Laptop GPU (6,141 MiB physical, 5,629 MiB allocatable, 512 MiB driver reserve). |
| **`cgpu-gpu-worker-3050`** | Subrat's Windows 11 Laptop (Ubuntu 24.04 WSL2) | `10.20.24.36` | **GPU Worker 2**. NVIDIA GeForce RTX 3050 Laptop GPU (4,096 MiB physical, 3,584 MiB allocatable, 512 MiB driver reserve). Stored on external drive `A:\`. |

**Total Aggregated GPU Pool:** **10,237 MiB VRAM** (9,213 MiB Allocatable after driver reserves).

---

## 3. What Was Built (Architecture Deep Dive)

The project was divided cleanly into two tracks:

### Track A: Infrastructure, Virtualization & Scheduling (Deep & Nishit)

#### 1. Cluster Foundation & WSL2 GPU Passthrough (Issues #16 & #17 — Deep Patel)
- **Lightweight Kubernetes**: We deployed K3s (`v1.37.2+k3s1`), which has a tiny memory footprint (<500MB) compared to standard vanilla Kubernetes, ideal for desktop and laptop worker nodes.
- **Solving WSL2 Networking**: Default WSL2 uses internal NAT (`172.24.x.x`), making worker pods unreachable from Ubuntu PCs. We solved this by enabling **mirrored networking** (`networkingMode=mirrored` in `%USERPROFILE%\.wslconfig`), binding WSL directly to the campus LAN subnet (`10.20.24.0/24`).
- **NVIDIA CDI & RuntimeClass**: Configured `nvidia-container-toolkit` with Container Device Interface (CDI) at `/etc/cdi/nvidia.yaml`. Declared Kubernetes `RuntimeClass: nvidia` so any pod specifying `runtimeClassName: nvidia` automatically receives passthrough access to `/dev/dxg`.
- **Validation**: Verified with containerized CUDA `vectorAdd` computing 50,000 vector elements inside Kubernetes pods.

#### 2. CUDA Allocation Guard (`pkg/guard/libcgpu_guard.so` — Deep Patel & Nishit Patel)
- **The Core Problem**: Consumer laptop GPUs running in WSL2 do NOT support NVIDIA vGPU partitioning, and `nvidia-smi` does not support per-process memory limits.
- **Our Solution**: We wrote a custom C dynamic library (`pkg/guard/cgpu_guard.c`).
- **How it Works**: When a container starts, `libcgpu_guard.so` is loaded via `LD_PRELOAD`. It hooks:
  - `cudaMalloc()` & `cudaFree()` (CUDA Runtime API)
  - `cuMemAlloc_v2()` & `cuMemFree_v2()` (CUDA Driver API)
  - `cudaMemGetInfo()` & `cuMemGetInfo_v2()`
- **Enforcement**: When a workload attempts to allocate more VRAM than its assigned `CGPU_ALLOCATED_VRAM_MIB` quota, the guard intercepts the call and **refuses it immediately before reaching the hardware**, cleanly returning `cudaErrorMemoryAllocation` / `CUDA_ERROR_OUT_OF_MEMORY`.
- **Virtualizing Capacity**: If PyTorch calls `cudaMemGetInfo()`, the guard lies and reports only the assigned quota (e.g., 1024 MiB) rather than the physical 6141 MiB card. This prevents PyTorch from allocating the whole physical card on startup.

#### 3. Central Placement Scheduler (`cmd/cgpu-scheduler/` — Nishit Patel)
- **Best-Fit Binpacking**: Evaluates all candidate GPU nodes and chooses the GPU that has the smallest remaining memory after allocation ($Free - Requested \ge 0$). This packs jobs tightly and leaves larger contiguous chunks free for bigger workloads.
- **Driver Context Overhead Reserve**: Reserves **512 MiB per GPU** automatically. Initializing the CUDA driver context on consumer GPUs immediately takes ~300-500 MiB of VRAM before user code runs; reserving this prevents over-subscription crashes.
- **Dynamic Queue**: If no GPU currently has enough free VRAM, the request enters a thread-safe `PENDING` queue and is automatically dispatched as soon as a running job releases memory.
- **REST Endpoints on Port 8082**:
  - `POST /api/v1/schedule` — submit placement request
  - `POST /api/v1/release` — release completed job quota
  - `GET /api/v1/pool` — get real-time pool capacity
  - `GET /metrics` — Prometheus metrics format

#### 4. RTX 3050 Join on External Drive A: & Observability (Issues #18 & #19 — Nishit Patel)
- Joined second GPU worker (`cgpu-gpu-worker-3050`) on drive `A:\WSL\Ubuntu-24.04.vhdx` to prevent consuming laptop internal C: storage.
- Real-time LAN Observability Dashboard (`scripts/dashboard.py` on `http://10.20.24.31:8081`).

---

### Track B: API Gateway, Portal & Demonstration (Nirav & Subrat)

#### 1. OpenAPI 3.0 Specification & Mock API Server (Issue #8 / PR #15 — Nirav Shah)
- Established formal API contract (`api/openapi/openapi.yaml`).
- Mock server (`api/mocks/server.py`) serving on port 8000 covering 9 distinct lifecycle states:
  1. Successful job submission
  2. Queued / waiting for capacity
  3. Invalid request rejection
  4. Telemetry unavailable
  5. Out-of-memory failure
  6. Completed job with checkpoint
  7. Live GPU inventory
  8. Job history
  9. Live job streaming logs

#### 2. End-to-End Demonstration Workload (`workloads/demonstration/` — Subrat Jain & Nirav Shah)
- **Workload**: PyTorch ResNet-18 training on CIFAR-10 image classification.
- **Multi-Tenancy Proof**: Sized for 2,048 MiB VRAM quota. Enables running **multiple concurrent training jobs** on a single RTX 4050 GPU simultaneously.
- Produces saved PyTorch checkpoint files (`model_final.pt`, `checkpoint_epoch_1.pt`) and JSON training loss metrics (`metrics.json`).

---

## 4. Step-by-Step Professor Demonstration Script

When presenting in front of the professor, follow this exact sequence:

```
[Projector Display]
Step 1: Open Chrome -> http://10.20.24.31:8081 (Realtime LAN Dashboard)
Step 2: Terminal 1  -> ./scripts/status.sh (Instant CLI Health Check)
Step 3: Chrome/Curl -> http://10.20.24.31:8082/metrics (Prometheus Telemetry)
Step 4: Terminal 2  -> curl -X POST http://10.20.24.31:8082/api/v1/schedule (Live Placement)
Step 5: Terminal 3  -> Run Quota Enforcement Test (Over-quota rejection)
Step 6: GitHub      -> Show Issues #16, #17, #18, #19 & commit history
```

### Step 1: Show the Live Web Dashboard (Visual Impact!)
1. Open your browser and navigate to:
   ```
   http://10.20.24.31:8081
   ```
2. **What to say to the professor:**
   > *"Professor, this is our real-time CGPU cluster dashboard running live on our control plane. As you can see, our cluster aggregates 4 nodes across our local university network: two Ubuntu desktop PCs and two Windows laptops running WSL2. Our aggregated GPU pool has 10,237 MiB of physical VRAM. The dashboard updates live via background pollers without refreshing the page."*
3. **What to point out on screen:**
   - Control plane (`cgpu-control-plane` at `10.20.24.31`) is `Ready`.
   - Both GPU workers (`cgpu-gpu-worker-4050` and `cgpu-gpu-worker-3050`) are `Ready`.
   - Milestones card shows: `Issues #16, #17, #18 & #19 Closed ✅`.

---

### Step 2: Instant Terminal Cluster Health Check
1. Open a terminal on your Mac/laptop (connected to the same Wi-Fi `10.20.24.x`):
   ```bash
   cd /Users/nishit/Desktop/Cloud/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform
   ./scripts/status.sh
   ```
2. **What it outputs:**
   - `kubectl get nodes -o wide` showing all 4 nodes `Ready`.
   - Running pods across all namespaces.
   - Live JSON output from the scheduler placement engine.
3. **What to say:**
   > *"This automated script verifies our control plane connectivity, flannel overlay networking, and scheduler telemetry across all 4 machines in one step."*

---

### Step 3: Show Prometheus Metrics Endpoint
1. In the browser or terminal, open:
   ```bash
   curl -s http://10.20.24.31:8082/metrics
   ```
2. **What to say:**
   > *"Our central scheduler exports standard Prometheus metrics on port 8082. It tracks total pooled nodes, physical VRAM, allocatable VRAM after our 512 MiB driver context reservation, active allocations, and pending queue length."*

---

### Step 4: Demonstrate Live Placement & Binpacking
1. Submit a 1,024 MiB job request to the live scheduler:
   ```bash
   curl -X POST http://10.20.24.31:8082/api/v1/schedule \
     -H "Content-Type: application/json" \
     -d '{"job_id":"demo-job-1","workload_id":"resnet18","requested_vram_mib":1024,"priority":5}'
   ```
2. **Expected Response:**
   ```json
   {
     "job_id": "demo-job-1",
     "status": "PLACED",
     "assigned_node": "cgpu-gpu-worker-3050",
     "assigned_gpu_id": "GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6",
     "quota": {"allocated_vram_mib": 1024, "enforcement_mode": "CUDA_INTERCEPTOR"},
     "placement_reason": "BEST_FIT_PACKING_FOUND"
   }
   ```
3. **What to say:**
   > *"Notice how the scheduler chose the RTX 3050 instead of the 4050! Because our best-fit binpacking algorithm selected the node with the tightest remaining fit, saving the larger 6 GB RTX 4050 for heavier workloads."*

4. Now release the allocation when done:
   ```bash
   curl -X POST http://10.20.24.31:8082/api/v1/release \
     -H "Content-Type: application/json" \
     -d '{"job_id":"demo-job-1"}'
   ```

---

### Step 5: Demonstrate VRAM Quota Enforcement (The Magic!)
1. SSH into the RTX 4050 laptop worker or run our test suite:
   ```bash
   bash test/e2e/gpu/guard_quota_test.sh
   ```
2. **What happens:**
   - A test process requests 128 MiB (allowed).
   - The process then requests another allocation that exceeds its quota.
   - The guard intercepts `cuMemAlloc_v2`, logs `deny [CUDA_ERROR_OUT_OF_MEMORY]`, and blocks it from hardware.
   - The workload safely handles the error without crashing the GPU or machine.
3. **What to say:**
   > *"This proves that our C interposer strictly enforces per-job VRAM limits without any hardware vGPU support from NVIDIA."*

---

### Step 6: Show GitHub Issues & Clean History
1. Open GitHub repo:
   `https://github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/issues?q=is%3Aissue+is%3Aclosed`
2. **Point out:**
   - **Issue A1 (#16)**: Cluster join & RTX 4050 WSL2 setup — Deep Patel (`@Deeppatel40`)
   - **Issue A2 (#17)**: Containerized CUDA validation — Deep Patel (`@Deeppatel40`)
   - **Issue A3 (#18)**: CGPU Scheduler & VRAM quota enforcement — Nishit Patel (`@Nishit1006`)
   - **Issue A4 (#19)**: RTX 3050 join & multi-GPU pool monitoring — Nishit Patel (`@Nishit1006`)
   - **Issue B1 (#8)**: OpenAPI contract & API mocks — Nirav Shah (`@NiravShah1729`)

---

## 5. Professor Q&A Cheat Sheet

Here are the questions your professor is most likely to ask, along with the exact technical answers:

#### Q1: "Why did you use K3s instead of standard upstream Kubernetes?"
> **Answer:** Standard Kubernetes (kube-apiserver, etcd, kubelet) consumes 2–4 GB of RAM just for the control plane and requires dedicated Linux distributions. K3s packages everything into a single lightweight binary (<500 MB RAM), uses SQLite/embedded storage instead of heavy etcd, and supports multi-node clustering effortlessly across consumer PCs and laptops.

#### Q2: "Consumer GPUs like RTX 4050 do not support NVIDIA vGPU. How did you achieve GPU memory pooling and sharing?"
> **Answer:** We implemented user-space CUDA API interposition. Our custom dynamic library `libcgpu_guard.so` is preloaded into the workload container via `LD_PRELOAD`. It intercepts `cudaMalloc()` and `cuMemAlloc_v2()`. When a job exceeds its scheduler-assigned VRAM quota, the guard rejects the allocation and returns a clean CUDA Out-of-Memory error before the request ever reaches the physical driver.

#### Q3: "WSL2 uses a virtual switch with private NAT. How did Ubuntu PCs communicate with the WSL2 laptops?"
> **Answer:** In default NAT mode, WSL2 gets an unreachable internal `172.24.x.x` address. We solved this by configuring **mirrored networking** (`networkingMode=mirrored` in `%USERPROFILE%\.wslconfig`). This mirrors the Windows host's physical network adapter into WSL2, giving the Linux guest a real, directly accessible IP on the `10.20.24.0/24` subnet.

#### Q4: "Why did you reserve 512 MiB of VRAM on each GPU for driver context overhead?"
> **Answer:** On NVIDIA consumer GPUs, simply initializing the CUDA driver context (loading runtime modules and page tables) consumes ~300 to 500 MiB of VRAM immediately before a single user tensor is allocated. If a scheduler allocates 100% of physical VRAM to workloads, the card crashes with an Out-of-Memory error during context initialization. Reserving 512 MiB guarantees stability.

#### Q5: "How does your scheduler decide which GPU gets which job?"
> **Answer:** We implemented a **Best-Fit Binpacking algorithm**. When a job requests $V$ MiB, the scheduler evaluates all GPUs with $Free - V \ge 0$ and selects the GPU that minimizes $(Free - V)$. This leaves larger GPUs available for bigger jobs and minimizes fragmentation. If no GPU has enough memory, the request enters a thread-safe pending queue.

#### Q6: "What happens if a student laptop's Wi-Fi IP address changes?"
> **Answer:** We deliberately did not hardcode `--node-ip` when installing the K3s agent. The K3s agent auto-detects its IP from the default routing table toward the control plane (`10.20.24.31`). If DHCP reassigns the lease, restarting the `k3s-agent` service automatically updates the node registration.

---

## 6. Emergency Troubleshooting Guide

If anything fails or disconnects right before or during your presentation, **do not panic**. Here are the 5-second fixes:

### 🔴 Problem 1: Web Dashboard (`:8081`) does not open in browser
- **Cause:** Python daemon on PC 1 stopped or restarted.
- **Fix (Run from your Mac or PC 1 terminal):**
  ```bash
  sshpass -p 'Nishit' ssh g6@10.20.24.31 "echo Nishit | sudo -S systemctl restart cgpu-dashboard"
  ```
- Check status:
  ```bash
  sshpass -p 'Nishit' ssh g6@10.20.24.31 "systemctl status cgpu-dashboard --no-pager"
  ```

---

### 🔴 Problem 2: Laptop GPU Worker shows `NotReady` or disappeared
- **Cause:** The Windows laptop went to sleep or the WSL2 session was closed. WSL terminates the Linux VM when all client terminal windows close!
- **Fix on Deep's or Subrat's Laptop:**
  1. Open Windows Terminal or Command Prompt.
  2. Type:
     ```cmd
     wsl
     ```
  3. Inside WSL, restart the agent:
     ```bash
     sudo systemctl restart k3s-agent
     ```
  4. Leave that terminal open in the background!

---

### 🔴 Problem 3: Scheduler API (`:8082`) returns connection refused
- **Cause:** `cgpu-scheduler` service on PC 1 needs a restart.
- **Fix:**
  ```bash
  sshpass -p 'Nishit' ssh g6@10.20.24.31 "echo Nishit | sudo -S systemctl restart cgpu-scheduler"
  ```
- Verify:
  ```bash
  curl http://10.20.24.31:8082/api/v1/pool
  ```

---

### 🔴 Problem 4: Laptop Wi-Fi IP changed
- **Check IP on Windows:**
  ```cmd
  ipconfig
  ```
- **Check IP inside WSL:**
  ```bash
  ip route get 10.20.24.31
  ```
- **Restart agent to register new IP:**
  ```bash
  sudo systemctl restart k3s-agent
  ```

---

### 🔴 Problem 5: "CUDA error: out of memory" during demo test
- **Note:** If this happens during the **Quota Enforcement Test**, that is **EXPECTED AND INTENDED**! That is the proof that our guard blocked an over-quota job! Explain to the professor: *"Look, that error was generated deliberately by our guard to prevent VRAM overflow!"*

---

## 7. Team Roles & Contributions

If the professor asks *"Who worked on what?"*:

| Member | GitHub Handle | Key Responsibilities & Deliverables |
| :--- | :--- | :--- |
| **Deep Patel** | `@Deeppatel40` | **Infrastructure & Guard Lead (Track A)**<br>• Configured K3s on Ubuntu PC 1 and joined RTX 4050 WSL2 worker (Issue #16 / A1).<br>• Implemented NVIDIA CDI containerized runtime & validated CUDA vectorAdd (Issue #17 / A2).<br>• Co-developed the C-based CUDA allocation guard (`libcgpu_guard.so`) for quota enforcement. |
| **Nishit Patel** | `@Nishit1006` | **Scheduler & Telemetry Lead (Track A)**<br>• Developed Go best-fit binpacking scheduler & placement daemon on port 8082 (Issue #18 / A3).<br>• Integrated 512 MiB driver context overhead reserve and workload handoff specs.<br>• Joined RTX 3050 GPU worker on drive A:, exported Prometheus metrics, and built the real-time LAN Dashboard (Issue #19 / A4). |
| **Nirav Shah** | `@NiravShah1729` | **API & Architecture Lead (Track B)**<br>• Authored OpenAPI 3.0 specification for end-to-end job submission lifecycle.<br>• Built Python mock API server (`api/mocks/server.py`) simulating all 9 job states (Issue #8 / B1 / PR #15).<br>• Defined TypeScript schema contracts for frontend integration (`portal/types/api.ts`). |
| **Subrat Jain** | `@subrat-j` (formerly `@CodexKnight-ai`) | **Workload & Integration Lead (Track B)**<br>• Packaged ResNet-18 CIFAR-10 demonstration training workload with PyTorch checkpointing.<br>• Configured external storage on Windows drive `A:\` for RTX 3050 WSL virtual disk.<br>• Documented end-to-end testing criteria, handoff validation, and project guides. |

---

## 8. Quick Command Cheat Sheet

Keep these commands handy during the presentation:

| Task | Command Line |
| :--- | :--- |
| **Full Cluster Status** | `./scripts/status.sh` |
| **Open Web Dashboard** | Open `http://10.20.24.31:8081` in Chrome |
| **View Prometheus Telemetry** | `curl -s http://10.20.24.31:8082/metrics` |
| **View Pool JSON** | `curl -s http://10.20.24.31:8082/api/v1/pool` |
| **Schedule Test Job (1GB)** | `curl -X POST http://10.20.24.31:8082/api/v1/schedule -H "Content-Type: application/json" -d '{"job_id":"test-1","workload_id":"resnet18","requested_vram_mib":1024,"priority":5}'` |
| **Release Job Allocation** | `curl -X POST http://10.20.24.31:8082/api/v1/release -H "Content-Type: application/json" -d '{"job_id":"test-1"}'` |
| **Run Quota Enforcement Test** | `bash test/e2e/gpu/guard_quota_test.sh` |
| **SSH to PC 1 Control Plane** | `sshpass -p 'Nishit' ssh g6@10.20.24.31` |
| **Restart Dashboard Service** | `sshpass -p 'Nishit' ssh g6@10.20.24.31 "echo Nishit \| sudo -S systemctl restart cgpu-dashboard"` |
| **Restart Scheduler Service** | `sshpass -p 'Nishit' ssh g6@10.20.24.31 "echo Nishit \| sudo -S systemctl restart cgpu-scheduler"` |

---
*Good luck with the demonstration! You have a fully working, multi-node, GPU-virtualized cloud platform running live on real hardware.*
