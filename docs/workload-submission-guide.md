# CGPU Workload Submission & Execution Reference (A3 Handoff)

**Audience:** Application Pair (Nirav Shah [@NiravShah1729](https://github.com/NiravShah1729) & Subrat Jain [@CodexKnight-ai](https://github.com/CodexKnight-ai))  
**Authors:** Nishit Patel ([@Nishit1006](https://github.com/Nishit1006)) & Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40))  
**Date:** 14 November 2026  
**Satisfies:** Criterion 9 of Issue #18 (A3)

---

## 1. Overview

This document provides the application team with the exact specification, Kubernetes Job template, and API state lifecycle to connect the Go Job Manager (Issue #12 / B2) and Next.js portal (Issue #13 / B3) to the running CGPU Scheduler and RTX 4050 / RTX 3050 GPU workers.

---

## 2. Scheduler API Integration

The CGPU Central Scheduler runs continuously on the Control Plane (`10.20.24.31:8082`).

### 2.1 Requesting Placement (`POST /api/v1/schedule`)

```json
{
  "job_id": "job-a1b2c3d4-e5f6-7890",
  "workload_id": "resnet18-cifar10",
  "requested_vram_mib": 1024,
  "priority": 5,
  "submitted_at": "2026-11-14T10:30:00Z"
}
```

### 2.2 Placement Decision Responses

#### Case A: Immediate Placement (`status: PLACED`)
```json
{
  "job_id": "job-a1b2c3d4-e5f6-7890",
  "status": "PLACED",
  "assigned_node": "cgpu-gpu-worker-4050",
  "assigned_gpu_id": "GPU-04280514-80a3-1dcf-1c5e-94e374365bef",
  "assigned_gpu_index": 0,
  "quota": {
    "allocated_vram_mib": 1024,
    "enforcement_mode": "CUDA_INTERCEPTOR",
    "compute_shares": 18
  },
  "placement_reason": "BEST_FIT_PACKING_FOUND",
  "evaluated_nodes": 1,
  "pool_free_vram_after_mib": 4605,
  "decision_timestamp": "2026-11-14T10:30:00.123Z"
}
```

#### Case B: Queued Due to Capacity (`status: PENDING`)
```json
{
  "job_id": "job-a1b2c3d4-e5f6-7890",
  "status": "PENDING",
  "pending_reason": "WAITING_FOR_CAPACITY",
  "evaluated_nodes": 1,
  "decision_timestamp": "2026-11-14T10:30:00.123Z"
}
```

#### Case C: Rejection (`status: REJECTED`)
```json
{
  "job_id": "job-a1b2c3d4-e5f6-7890",
  "status": "REJECTED",
  "rejection_reason": "REQUESTED_VRAM_EXCEEDS_MAX_HARDWARE",
  "evaluated_nodes": 1,
  "decision_timestamp": "2026-11-14T10:30:00.123Z"
}
```

### 2.3 Releasing Capacity (`POST /api/v1/release`)

Dispatched when a job finishes or fails:
```json
{
  "job_id": "job-a1b2c3d4-e5f6-7890",
  "node_id": "cgpu-gpu-worker-4050",
  "gpu_id": "GPU-04280514-80a3-1dcf-1c5e-94e374365bef",
  "vram_mib": 1024
}
```

---

## 3. Kubernetes Job Manifest Template

When `status == PLACED`, instantiate the template located at [`deploy/templates/cgpu-workload-template.yaml`](../deploy/templates/cgpu-workload-template.yaml):

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: cgpu-job-a1b2c3d4
  namespace: default
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      nodeName: cgpu-gpu-worker-4050
      runtimeClassName: nvidia
      hostNetwork: true
      dnsPolicy: Default
      containers:
        - name: workload
          image: your-approved-workload:tag
          env:
            - name: CGPU_JOB_ID
              value: "job-a1b2c3d4"
            - name: CGPU_ALLOCATED_VRAM_MIB
              value: "1024"
            - name: NVIDIA_VISIBLE_DEVICES
              value: "all"
            - name: NVIDIA_DRIVER_CAPABILITIES
              value: "compute,utility"
            - name: LD_PRELOAD
              value: "/opt/cgpu/libcgpu_guard.so"
            - name: CGPU_GUARD_STATUS_FILE
              value: "/var/run/cgpu/guard/job-a1b2c3d4.json"
          volumeMounts:
            - name: cgpu-guard
              mountPath: /opt/cgpu
              readOnly: true
            - name: cgpu-status
              mountPath: /var/run/cgpu/guard
      volumes:
        - name: cgpu-guard
          hostPath:
            path: /opt/cgpu
            type: DirectoryOrCreate
        - name: cgpu-status
          hostPath:
            path: /var/run/cgpu/guard
            type: DirectoryOrCreate
```

### 4. Critical Guardrails for Track B

1. **Do NOT inject `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True`:** As demonstrated on the physical RTX 4050, PyTorch expandable segments use runtime dynamic symbol loading that bypasses `LD_PRELOAD` hooks. Strip or omit this variable so quotas remain strictly enforced.
2. **Workload Dynamic Linking:** Only container images dynamically linked against `libcuda.so` / `libcudart.so` can be policed by the interposer.
3. **WSL Worker Telemetry:** For jobs running on WSL GPU workers, per-job VRAM usage is read from the `/var/run/cgpu/guard/<job-id>.json` status file exported by our guard, as WSL driver telemetry does not support per-process accounting.
