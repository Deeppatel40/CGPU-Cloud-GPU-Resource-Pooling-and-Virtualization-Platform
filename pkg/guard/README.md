# CGPU CUDA Allocation Guard

CGPU's own `LD_PRELOAD` interposer for enforcing a per-workload VRAM quota.

**Owner:** Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40)) — Issue A3 (#18), Package 1.

Not derived from HAMi or HAMi-Core. No HAMi code, headers or resource field names are used.

## Why a guard at all

On this project's WSL GPU workers the driver does not report per-process VRAM usage: `--query-compute-apps used_memory` returns `[N/A]`, `nvidia-smi pmon` is unusable, and accounting mode is `Disabled` (see [`docs/gpu-worker-4050.md`](../../docs/gpu-worker-4050.md)). There is therefore **no external way to observe what a single job is using**, let alone cap it.

So the guard's own tally is the source of truth. It does three things:

1. **Enforces** — intercepts the allocation and free paths, refuses anything that would push the process past its quota, and returns the usual out-of-memory error *without* calling the real allocator, so nothing is reserved on the card for a denied request.
2. **Virtualises capacity** — reports the quota as the device's total and free memory, so frameworks size their pools to the quota instead of to the physical card. Without this a framework sizes from the real card and burns its whole quota before failing.
3. **Reports** — writes per-job usage to a status file, which is the only per-job VRAM signal available on these workers and what `cmd/cgpu-worker-agent` exports for A4.

## Build

```bash
./build.sh [output-dir]      # default: ./build
```

No CUDA toolkit required: the CUDA symbols and error codes are declared in the source and the real implementations are resolved at runtime. Only `gcc` and libc headers are needed.

The build **fails deliberately** if the library requires a newer glibc than the workload images provide (ceiling 2.35 = Ubuntu 22.04; override with `CGPU_TARGET_GLIBC`). A library needing a newer glibc cannot load in the containers it exists to govern, and the failure would otherwise only surface at runtime inside a container. The same constraint is why the source parses its integer environment variables by hand rather than calling `strtoull()`, whose modern `__isoc23_` redirect carries a `GLIBC_2.38` requirement.

## Use

```bash
LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
CGPU_ALLOCATED_VRAM_MIB=512 \
CGPU_JOB_ID=job-1234 \
  <workload>
```

| Variable | Default | Meaning |
| --- | --- | --- |
| `CGPU_ALLOCATED_VRAM_MIB` | — | quota in MiB, as assigned by the scheduler |
| `CGPU_ALLOCATED_VRAM_BYTES` | — | exact-byte quota; overrides the above, used by tests |
| `CGPU_JOB_ID` | `-` | job identifier, included in every log line |
| `CGPU_GUARD_STATUS_FILE` | — | if set, current usage is written here for telemetry |
| `CGPU_GUARD_VERBOSE` | `0` | `1` logs every allocation and free, not only denials |
| `CGPU_GUARD_VIRTUALIZE_MEMINFO` | `1` | report the quota as device capacity |
| `CGPU_GUARD_HOOK_DLSYM` | `0` | **experimental**, see the warning below |

With no quota configured the guard stays in **pass-through** mode and only logs that fact, so preloading it can never break an unquotaed workload.

Status file format — the per-job usage signal the driver cannot give us:

```text
job_id job-1234
used_bytes 402653184
peak_bytes 536870912
quota_bytes 536870912
allocations 3
denials 1
```

## Covered CUDA paths

Required by A3 criterion 5: *"Record which CUDA paths are covered and verify coverage for the intended demo workload separately."*

### Enforced

| Symbol | API | Note |
| --- | --- | --- |
| `cudaMalloc` | Runtime | PyTorch's default allocator |
| `cudaMallocManaged` | Runtime | |
| `cudaMallocAsync` | Runtime | pool allocator, `backend:cudaMallocAsync` |
| `cudaFree`, `cudaFreeAsync` | Runtime | |
| `cuMemAlloc_v2` | Driver | |
| `cuMemAllocManaged` | Driver | |
| `cuMemAllocAsync` | Driver | |
| `cuMemAllocFromPoolAsync` | Driver | |
| `cuMemCreate`, `cuMemRelease` | Driver | virtual memory API; physical commit point |
| `cuMemFree_v2`, `cuMemFreeAsync` | Driver | |

### Virtualised (reported as the quota)

`cuMemGetInfo_v2`, `cudaMemGetInfo`, `cuDeviceTotalMem_v2`. A workload under a 512 MiB quota sees a 512 MiB GPU with 512 MiB free. Verified: `torch.cuda.mem_get_info()` reports `total=512MiB free=512MiB`.

### Not covered

- **Shaped allocations** — `cudaMallocPitch`, `cudaMalloc3D`, `cudaMallocArray`, `cuMemAllocPitch_v2`. Not used by the approved workloads.
- **Host-side memory** — `cudaMallocHost`, `cuMemHostAlloc`. These allocate host memory, which the VRAM quota does not govern.
- **Anything resolved at runtime by name** — see below.

## Two limitations that change how CGPU must be operated

### 1. Static linking defeats interposition entirely

A workload is only governable if it resolves CUDA symbols **through the dynamic linker**. Verified on this worker: `nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0` statically links the CUDA runtime and reaches the driver via `dlsym()`. Its binary has **zero undefined CUDA symbols**, so `LD_PRELOAD` has nothing to bind — the guard loads, reports itself active, and the workload then allocates **completely unpoliced**.

Detection signal: a workload that logs `guard library loaded via LD_PRELOAD` but produces **no** allocation lines under `CGPU_GUARD_VERBOSE=1` is unpoliced.

This is why [`test/e2e/gpu/cgpu_alloc_probe.c`](../../test/e2e/gpu/cgpu_alloc_probe.c) exists rather than reusing a stock sample: it links `libcuda` dynamically, the way real framework workloads do.

### 2. `expandable_segments:True` escapes the quota

Measured: with `PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True`, a 1024 MiB tensor against a 512 MiB quota **succeeded**, reserving 1040 MiB with zero guard denials. PyTorch resolves the virtual memory API by name at runtime, so those calls never reach the wrappers.

**`CGPU_GUARD_HOOK_DLSYM` exists to close this and is off by default.** It interposes `dlsym` and hands back guarded wrappers. It does close the gap, but the driver resolves *its own internals* the same way, so the hook makes libcuda re-enter this library. On the RTX 4050 it first caused every allocation to be charged twice — a single 64 MiB request consumed 128 MiB of quota — and once that was fixed with a re-entrancy counter it hung the workload outright, while the same probe ran cleanly with the hook disabled. Do not enable it without a way to distinguish driver-internal lookups from workload ones.

**So the mitigation belongs in the scheduler:** strip or pin `PYTORCH_CUDA_ALLOC_CONF` on submitted jobs. This is verified in `bypass_attempt_test.sh` — with the variable not passed through, the same 1024 MiB request is refused.

### What the guard is, and is not

It enforces quotas for **cooperative and approved** workloads. It is **not a security boundary**: a workload determined to escape can reach the driver's symbols by other means, and an in-process interposer cannot promise otherwise. CGPU should restrict submissions to an approved workload list whose linkage has been verified, and sanitise job environments.

## Tests

| Test | Criterion |
| --- | --- |
| [`guard_quota_test.sh`](../../test/e2e/gpu/guard_quota_test.sh) | 7 — an over-quota allocation is refused |
| [`quota_isolation_test.sh`](../../test/e2e/gpu/quota_isolation_test.sh) | 7 — denied job while a neighbour continues |
| [`concurrent_share_test.sh`](../../test/e2e/gpu/concurrent_share_test.sh) | 6 — two jobs, distinct quotas, overlapping progress |
| [`pytorch_coverage_test.sh`](../../test/e2e/gpu/pytorch_coverage_test.sh) | 5 — coverage for the demo workload |
| [`bypass_attempt_test.sh`](../../test/e2e/gpu/bypass_attempt_test.sh) | 5 — bypass attempts and the mitigation |

Results are recorded in [`docs/a3-guard-evidence.md`](../../docs/a3-guard-evidence.md).
