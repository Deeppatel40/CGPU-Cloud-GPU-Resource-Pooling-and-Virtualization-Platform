# CGPU Scheduler & Placement Component Handoff Specification

- **Target Audience**: Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40)), Nishit ([@Nishit1006](https://github.com/Nishit1006)), Nirav Shah ([@NiravShah1729](https://github.com/NiravShah1729)), Subrat Jain ([@CodexKnight-ai](https://github.com/CodexKnight-ai))
- **Related Issue**: [#8 (Issue B1: Job API Contract)](https://github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/issues/8)
- **Status**: Formatted for Collaboration Agreement

---

## 1. Context & Motivation

In Issue #16 (Issue A1), Deep bring up the Kubernetes cluster and register worker nodes, including the RTX 4050 6GB Laptop GPU running under WSL2.
In Issue #8 (Issue B1), we establish the formal handoff contract between the Go API Gateway / Job Manager and the CGPU Placement Component / Scheduler.

This contract ensures that:
1. The Go Job Manager passes normalized resource requests to the scheduler.
2. The Placement Component evaluates GPU inventory, applies bin-packing / fractional virtualization rules, and returns an atomic placement decision.
3. The Kubernetes Pod or Container is injected with the exact GPU virtualization quota configuration.

---

## 2. Interface Contract Overview

```text
  [ Go API Job Manager ]
           │
           │  1. PlacementRequest (job_id, requested_vram_mib, priority...)
           ▼
  [ CGPU Placement Engine ]
     │
     ├── 2. Reads GPU Inventory State (Mongo + In-Memory Pool)
     ├── 3. Applies Placement Policy (Best-Fit / Bin-Packing)
     │
     ▼  4. Decision Branch
     ├── If Available:
     │     - Atomically increments GPU allocated_vram_mib
     │     - Returns PlacementDecision (Status: PLACED, Node, GPU ID, Quota)
     │     - Triggers Kubernetes Pod Manifest generation
     │
     └── If Insufficient Capacity:
           - Returns PlacementDecision (Status: PENDING, PendingReason)
           - Enqueues job in priority/FIFO waitlist
```

---

## 3. Data Structures

### 3.1 `PlacementRequest`
Dispatched by the Job Manager to request GPU placement:

| Field | Type | Required | Description |
| :--- | :--- | :--- | :--- |
| `job_id` | `string` | Yes | Unique identifier of the job (e.g. `job-9e2b14f8-45a1...`) |
| `workload_id` | `string` | Yes | Registered workload catalog name (e.g. `pytorch-resnet18-cifar10`) |
| `requested_vram_mib` | `int` | Yes | Requested VRAM as a positive integer in MiB (e.g. `2048`) |
| `priority` | `int` | Yes | Priority integer between `1` (lowest) and `10` (highest). Default: `5` |
| `node_selector` | `map[string]string` | No | Optional node labels (e.g. `{"kubernetes.io/hostname": "wsl-worker-rtx4050-01"}`) |
| `required_cuda_version`| `string` | No | Minimum CUDA capability or version string (e.g. `>=12.0`) |
| `submitted_at` | `time.Time` | Yes | Timestamp of original user submission |

### 3.2 `PlacementDecision`
Returned by the Placement Component back to the Job Manager:

| Field | Type | Description |
| :--- | :--- | :--- |
| `job_id` | `string` | Matches the requested `job_id` |
| `status` | `string` | `PLACED`, `PENDING`, or `REJECTED` |
| `assigned_node` | `string` | Kubernetes Node name (e.g. `wsl-worker-rtx4050-01`) |
| `assigned_gpu_id` | `string` | Unique hardware/pool identifier of the GPU (e.g. `gpu-nv-rtx4050-c89b3f`) |
| `assigned_gpu_index`| `int` | Physical GPU index on the target node (`0` for single GPU systems) |
| `quota` | `GPUQuota` | Allocated resource parameters and virtualization enforcement mode |
| `placement_reason` | `string` | Standardized reason code when `status == PLACED` |
| `pending_reason` | `string` | Standardized reason code when `status == PENDING` |
| `evaluated_nodes` | `int` | Number of candidate worker nodes evaluated |
| `decision_timestamp` | `time.Time` | Timestamp of placement calculation |

### 3.3 `GPUQuota`
Specifies how the container runtime enforces fractional limits:

```go
type GPUQuota struct {
    AllocatedVRAMMiB int    `json:"allocated_vram_mib"` // Positive integer in MiB
    EnforcementMode  string `json:"enforcement_mode"`   // CGPU_VIRTUAL_CGROUP | NVIDIA_MPS | CUDA_INTERCEPTOR
    ComputeShares    int    `json:"compute_shares"`     // Compute percentage (e.g. 50% for 2048 MiB on 4050)
}
```

---

## 4. Standardized Reason Codes

### 4.1 Placement Reasons (`status: PLACED`)
- `BEST_FIT_PACKING_FOUND`: Placed on GPU with optimal remaining memory footprint to maximize overall pool utilization.
- `EXCLUSIVE_ALLOCATION`: Placed exclusively on a dedicated GPU device.
- `NODE_AFFINITY_MATCHED`: Placed on explicitly requested worker node.

### 4.2 Pending Reasons (`status: PENDING`)
- `WAITING_FOR_CAPACITY`: Aggregate pool capacity is below requested VRAM or no single GPU has sufficient contiguous unallocated VRAM.
- `QUEUED_FIFO`: Placed in scheduling queue behind higher-priority or earlier jobs.
- `NODE_DRAINING_OR_CORDONED`: Candidate node is temporarily in maintenance or offline.
- `SCHEDULING_IN_PROGRESS`: Currently being processed by the placement cycle.

### 4.3 Rejection Reasons (`status: REJECTED`)
- `REQUESTED_VRAM_EXCEEDS_MAX_HARDWARE`: Requested VRAM (e.g. `8192` MiB) exceeds the largest physical GPU in the pool (e.g. RTX 4050 has `6144` MiB).
- `NO_ELIGIBLE_NODES`: No active nodes match the specified node selectors or CUDA requirements.

---

## 5. Kubernetes Pod Injection Contract

When the placement decision is `PLACED`, the placement component outputs the following environment variables and pod annotations to the Kubernetes Pod template:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cgpu-job-9e2b14f8-45a1-432c-b58a-6b5832a8e412
  namespace: cgpu-workloads
  annotations:
    cgpu.io/job-id: "job-9e2b14f8-45a1-432c-b58a-6b5832a8e412"
    cgpu.io/gpu-id: "gpu-nv-rtx4050-c89b3f"
    cgpu.io/allocated-vram-mib: "2048"
    cgpu.io/enforcement-mode: "CGPU_VIRTUAL_CGROUP"
spec:
  nodeName: wsl-worker-rtx4050-01
  containers:
  - name: workload
    image: cgpu/workloads/pytorch-resnet18-cifar10:1.0.0
    env:
    - name: NVIDIA_VISIBLE_DEVICES
      value: "0"
    - name: CGPU_ALLOCATED_VRAM_MIB
      value: "2048"
    - name: CGPU_TOTAL_GPU_VRAM_MIB
      value: "6144"
    - name: CGPU_ENFORCEMENT_MODE
      value: "CGPU_VIRTUAL_CGROUP"
    - name: PYTORCH_CUDA_ALLOC_CONF
      value: "max_split_size_mb:128"
    resources:
      limits:
        nvidia.com/gpu: 1
```

---

## 6. Agreement Sign-off Checklist

- [x] **Submission VRAM**: Integer in MiB (`requested_vram_mib: 2048`)
- [x] **Placement Node ID**: String matching K8s `spec.nodeName` (e.g. `wsl-worker-rtx4050-01`)
- [x] **Hardware Identifier**: String hardware ID (e.g. `gpu-nv-rtx4050-c89b3f`)
- [x] **Quota Details**: `allocated_vram_mib`, `enforcement_mode`, `compute_shares`
- [x] **Pending and Placement Reason Enums**: Explicit enumerated strings defined
- [x] **Atomic Reservation Protocol**: Documented and verified

