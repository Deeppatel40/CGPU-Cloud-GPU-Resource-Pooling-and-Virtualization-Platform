# CGPU CUDA Allocation Guard

CGPU's own `LD_PRELOAD` interposer for enforcing a per-workload VRAM quota.

**Owner:** Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40)) — Issue A3 (#10), Package 1.

Not derived from HAMi or HAMi-Core. No HAMi code, headers or resource field names are used.

## Why a guard at all

On this project's WSL GPU workers the driver does not report per-process VRAM usage: `--query-compute-apps used_memory` returns `[N/A]`, `nvidia-smi pmon` is unusable, and accounting mode is `Disabled` (see [`docs/gpu-worker-4050.md`](../../docs/gpu-worker-4050.md)). There is therefore **no external way to observe what a single job is using**, let alone cap it.

So the guard's own tally is the source of truth. It intercepts the allocation and free paths, keeps a running total of device memory the process holds, and refuses any allocation that would cross the quota — returning the same error code the CUDA APIs return when the device is out of memory. The real allocator is never called for a denied request, so nothing is reserved on the card. Device-level `nvidia-smi` remains useful only as a sanity check on the total.

## Build

```bash
./build.sh [output-dir]      # default: ./build
```

No CUDA toolkit is required. The CUDA symbols and error codes are declared in the source and the real implementations are resolved at runtime with `dlsym(RTLD_NEXT, ...)`. Only `gcc` and libc headers are needed, which matters because host disk space on the 4050 laptop is tight.

The build **fails deliberately** if the library requires a newer glibc than the workload images provide (ceiling 2.35, Ubuntu 22.04; override with `CGPU_TARGET_GLIBC`). This is not pedantry — a library that needs a newer glibc cannot load in the very containers it exists to govern, and the failure would otherwise only appear at runtime inside a container. The same constraint is why the source parses its two integer environment variables by hand instead of calling `strtoull()`, whose modern `__isoc23_` redirect carries a `GLIBC_2.38` requirement.

## Use

```bash
LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
CGPU_ALLOCATED_VRAM_MIB=512 \
CGPU_JOB_ID=job-1234 \
  <workload>
```

| Variable | Meaning |
| --- | --- |
| `CGPU_ALLOCATED_VRAM_MIB` | quota in MiB, as assigned by the scheduler |
| `CGPU_ALLOCATED_VRAM_BYTES` | exact-byte quota; overrides the above, used by tests |
| `CGPU_JOB_ID` | job identifier, included in every log line |
| `CGPU_GUARD_STATUS_FILE` | if set, current usage is written here for telemetry |
| `CGPU_GUARD_VERBOSE` | `1` logs every allocation and free, not only denials |

With no quota configured the guard stays in **pass-through** mode and only logs that fact, so preloading it can never break an unquotaed workload.

The status file is the per-job usage signal that the driver cannot give us:

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

### Covered

| Symbol | API |
| --- | --- |
| `cudaMalloc` | Runtime |
| `cudaMallocManaged` | Runtime |
| `cudaFree` | Runtime |
| `cuMemAlloc_v2` | Driver |
| `cuMemAllocManaged` | Driver |
| `cuMemFree_v2` | Driver |

### Not covered

These are deliberate gaps, not oversights. Each would need its own interposer, and none is used by the controlled test workload:

- **Stream-ordered / pool allocations** — `cuMemAllocAsync`, `cuMemAllocFromPoolAsync`, `cudaMallocAsync`. Newer CUDA runtimes may route `cudaMalloc` through a memory pool, in which case the allocation bypasses the guard.
- **Virtual memory management API** — `cuMemCreate`, `cuMemMap`, `cuMemAddressReserve`.
- **Shaped allocations** — `cudaMallocPitch`, `cudaMalloc3D`, `cudaMallocArray`, `cuMemAllocPitch_v2`.
- **Host-side memory** — `cudaMallocHost`, `cuMemHostAlloc`. These allocate host memory, which is not what the VRAM quota governs.

### The important limitation: static linking defeats interposition

A workload is only governable by this guard if it resolves CUDA symbols **through the dynamic linker**. Verified on this worker: the `nvcr.io/nvidia/k8s/cuda-sample:vectoradd-cuda12.5.0` image statically links the CUDA runtime and reaches the driver via `dlsym()`. Its binary has **zero undefined CUDA symbols**, so `LD_PRELOAD` has nothing to bind — the guard loads, reports itself active, and the workload then allocates freely, completely unpoliced.

This is why [`test/e2e/gpu/cgpu_alloc_probe.c`](../../test/e2e/gpu/cgpu_alloc_probe.c) exists rather than reusing a stock sample: it links `libcuda` dynamically, the way real framework workloads do.

**Consequence for the scheduler:** a quota is only an enforcement boundary for dynamically linked workloads. CGPU must therefore either restrict submissions to an approved workload list whose linkage has been verified, or treat the quota as advisory for anything else and say so. The guard logs `guard library loaded via LD_PRELOAD` on load and one line per allocation under `CGPU_GUARD_VERBOSE=1`; a workload that shows the load line but no allocation lines is **unpoliced**, and that is the signal to check for.

### Still to verify separately

PyTorch — the intended demo workload — dynamically links `libcudart.so` and `libcuda.so.1`, so it is expected to be covered, **but this has not yet been confirmed on hardware**. The 4050 laptop's Windows `C:` drive had 0.2 GB free, and pulling the ~3 GB PyTorch image took the WSL VM down mid-pull. This verification is outstanding and must be done before A3 is considered complete. It specifically needs to establish whether PyTorch's caching allocator uses `cudaMalloc` or the async pool API, since the latter is not covered.

## Tests

| Test | Criterion |
| --- | --- |
| [`test/e2e/gpu/guard_quota_test.sh`](../../test/e2e/gpu/guard_quota_test.sh) | 7 — an over-quota allocation is refused while another workload continues |
| [`test/e2e/gpu/concurrent_share_test.sh`](../../test/e2e/gpu/concurrent_share_test.sh) | 6 — two jobs with distinct quotas make overlapping progress on one GPU |

Both run through the `nvidia` runtime in a container, which is the path CGPU uses in production. Results are recorded in [`docs/a3-guard-evidence.md`](../../docs/a3-guard-evidence.md).
