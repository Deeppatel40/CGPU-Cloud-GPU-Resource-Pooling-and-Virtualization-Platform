# GPU Worker: RTX 3050 (WSL 2) — Engineering Record

**Node:** `cgpu-gpu-worker-3050`  
**Owner:** Nishit Patel ([@Nishit1006](https://github.com/Nishit1006))  
**Covers:** Issue A4 (#19)  
**Date:** 14 November 2026  

Authoritative engineering record for the second consumer GPU worker joined to the CGPU Kubernetes cluster.

---

## 1. Hardware & Platform Identification

| Field | Value |
| --- | --- |
| GPU | NVIDIA GeForce RTX 3050 Laptop GPU |
| **Device identifier (UUID)** | `GPU-7e8adcd7-96a2-1fa6-7936-445a0f2643e6` |
| PCI bus | `00000000:01:00.0` |
| Total VRAM | 4096 MiB |
| Compute capability | 8.6 (Ampere) |
| Driver (KMD) | 616.92 |
| NVRM / NVIDIA-SMI | 615.71.08 |
| CUDA UMD | 13.4 |
| Host OS | Windows 11 Home Single Language (Build 22631.4391) |
| Guest OS | Ubuntu 24.04.3 LTS on WSL 2 (kernel `6.18.58.1-microsoft-standard-WSL2`) |
| Storage Location | **Strictly `A:\` drive** (`A:\WSL\Ubuntu-24.04.vhdx` on Micron NVMe SSD) |
| Node IP Address | `10.20.24.36/24` (Physical LAN via WSL mirrored networking) |
| K3s Version | agent `v1.37.2+k3s1`, containerd `2.3.7-k3s1` |
| Container Toolkit | `nvidia-container-toolkit` 1.20.2, CDI v0.3.0 (`/etc/cdi/nvidia.yaml`) |

---

## 2. Host Storage Constraint & Configuration

Because the host `C:\` partition had limited remaining capacity (~13 GB free), all persistent WSL data, virtual disks, and container images were placed onto the larger `A:\` drive (~77 GB free):
- VHDX Path: `A:\WSL\Ubuntu-24.04.vhdx` (imported into WSL via `wsl.exe --import-in-place`).
- WSL Mirrored Networking & Resource Quotas (`.wslconfig`):
  ```ini
  [wsl2]
  networkingMode=mirrored
  memory=4GB
  processors=4
  swap=4GB

  [experimental]
  autoMemoryReclaim=gradual
  ```
- **Logon Keep-Alive Task:** Registered scheduled task `CGPU-WSL-KeepAlive` executing `wsl.exe -d Ubuntu --exec sleep infinity` to guarantee the WSL VM remains active and connected to the cluster across user sessions.

---

## 3. Cluster Join & Runtime Verification

1. Cleaned up standalone local `k3s` server artifacts (`/usr/local/bin/k3s-uninstall.sh`).
2. Generated NVIDIA Container Device Interface (CDI) specification:
   ```bash
   sudo mkdir -p /etc/cdi
   sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
   ```
3. Installed K3s agent connected to the control plane (`https://10.20.24.31:6443`):
   ```bash
   curl -sfL https://get.k3s.io | K3S_URL=https://10.20.24.31:6443 K3S_TOKEN=<node-token> K3S_NODE_NAME=cgpu-gpu-worker-3050 sh -
   ```
4. K3s auto-detected NVIDIA container runtime and registered `nvidia` in `/var/lib/rancher/k3s/agent/etc/containerd/config.toml`.
5. Applied node labels:
   - `cgpu.io/role=gpu-worker`
   - `cgpu.io/gpu-model=rtx3050`
   - `cgpu.io/gpu-vram=4096MiB`
   - `node-role.kubernetes.io/worker=worker`

---

## 4. CUDA Smoke Test

Verified containerized CUDA computation on the RTX 3050 using both containerd ctr and a Kubernetes batch Job:

### K8s Job: `cuda-smoke-test-3050`
```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: cuda-smoke-test-3050
spec:
  template:
    spec:
      nodeName: cgpu-gpu-worker-3050
      runtimeClassName: nvidia
      hostNetwork: true
      dnsPolicy: Default
      restartPolicy: Never
      containers:
      - name: cuda-vectoradd
        image: nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0
        env:
        - name: NVIDIA_VISIBLE_DEVICES
          value: "all"
        - name: NVIDIA_DRIVER_CAPABILITIES
          value: "compute,utility"
```

### Observed Execution Log:
```text
[Vector addition of 50000 elements]
Copy input data from the host memory to the CUDA device
CUDA kernel launch with 196 blocks of 256 threads
Copy output data from the CUDA device to the host memory
Test PASSED
Done
```
Job status: `Completed` (Exit code: 0).

---

## 5. Multi-GPU Cluster Pool & Best-Fit Binpacking

With both GPU workers online, the aggregate cluster pool comprises:
- `cgpu-gpu-worker-4050`: RTX 4050 6141 MiB (Allocatable: 5629 MiB)
- `cgpu-gpu-worker-3050`: RTX 3050 4096 MiB (Allocatable: 3584 MiB)
- **Total Physical VRAM:** 10,237 MiB (~10.0 GB)
- **Total Allocatable VRAM:** 9,213 MiB (accounting for 2 × 512 MiB driver context reserves)

### Placement Test Results:
1. **Best-Fit Target (3000 MiB request):**
   - Evaluating RTX 4050 leaves `5629 - 3000 = 2629 MiB` free.
   - Evaluating RTX 3050 leaves `3584 - 3000 = 584 MiB` free.
   - Scheduler selected `cgpu-gpu-worker-3050` (`placement_reason: BEST_FIT_PACKING_FOUND`).
2. **Selective Capacity Constraint (4500 MiB request):**
   - RTX 3050 has only 3584 MiB allocatable (insufficient).
   - Scheduler selected `cgpu-gpu-worker-4050`.

---

## 6. Telemetry & Metrics Export

The CGPU Central Placement Daemon exports cluster and GPU device metrics:
- **Prometheus Scrape Endpoint:** `http://10.20.24.31:8082/metrics`
  - `cgpu_pool_total_nodes`: 2
  - `cgpu_pool_total_gpus`: 2
  - `cgpu_pool_vram_total_mib`: 10237
  - `cgpu_pool_vram_allocatable_mib`: 9213
  - `cgpu_device_vram_total_mib{model="rtx4050"}`: 6141
  - `cgpu_device_vram_total_mib{model="rtx3050"}`: 4096
- **Web Dashboard:** `http://10.20.24.31:8081` updates every 2 seconds, showing all 4 cluster nodes and live GPU pool allocations.
