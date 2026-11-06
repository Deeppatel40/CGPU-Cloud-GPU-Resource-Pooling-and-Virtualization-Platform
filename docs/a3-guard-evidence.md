# A3 Evidence — GPU sharing and VRAM quota enforcement (RTX 4050 worker)

**Engineer:** Deep Patel ([@Deeppatel40](https://github.com/Deeppatel40))
**Node:** `cgpu-gpu-worker-4050` — NVIDIA GeForce RTX 4050 Laptop GPU, `GPU-04280514-80a3-1dcf-1c5e-94e374365bef`, 6141 MiB
**Issue:** A3 (#10), Package 1 — criteria 6 and 7
**Captured:** 14 November 2026

Both tests run containerized through the `nvidia` runtime, which is the path CGPU uses in production. Reproduce with:

```bash
pkg/guard/build.sh /tmp/cgpu-guard-build
test/e2e/gpu/build.sh /tmp/cgpu-probe-build
test/e2e/gpu/guard_quota_test.sh        # criterion 7
test/e2e/gpu/concurrent_share_test.sh   # criterion 6
```

---

## Criterion 7 — quota enforcement

> *Cause one test workload to attempt an allocation above its quota. Verify an allocation error while the other workload continues. Use a request that would otherwise fit in physical memory so the result demonstrates quota enforcement.*

Setup: quota **128 MiB**, workload requests **256 MiB** in 64 MiB chunks, with **5920 MiB physically free**. The full request would have fitted on the card more than twenty times over, so a refusal can only be the quota.

```text
quota=128 MiB  request=256 MiB  physically free=5920 MiB
=> the full request fits in physical memory, so any denial can only be the quota

[cgpu-guard] info job=a3-c7-quota guard active quota_bytes=134217728 (128.0 MiB)
[cgpu-guard] info job=a3-c7-quota guard library loaded via LD_PRELOAD
[quota-enforced] device="NVIDIA GeForce RTX 4050 Laptop GPU"
[quota-enforced] uuid=GPU-04280514-80a3-1dcf-1c5e-94e374365bef
[cgpu-guard] allow job=a3-c7-quota cuMemAlloc_v2 size=67108864 used=67108864  quota=134217728
[quota-enforced] alloc    64 MiB  OK            total_held=64 MiB
[cgpu-guard] allow job=a3-c7-quota cuMemAlloc_v2 size=67108864 used=134217728 quota=134217728
[quota-enforced] alloc    64 MiB  OK            total_held=128 MiB
[cgpu-guard] deny  job=a3-c7-quota cuMemAlloc_v2 size=67108864 used=134217728 quota=134217728 would_exceed_by=67108864 -> returning out-of-memory
[quota-enforced] alloc    64 MiB  REFUSED rc=2  total_held=128 MiB (out of memory)
[cgpu-guard] free  job=a3-c7-quota cuMemFree_v2 size=67108864 used=67108864
[cgpu-guard] free  job=a3-c7-quota cuMemFree_v2 size=67108864 used=0
[quota-enforced] freed 2 chunks (128 MiB)
[quota-enforced] RESULT=OK (denial observed as expected)
```

All assertions passed:

```text
PASS: guard loaded inside the container
PASS: guard enforced the assigned quota
PASS: allocations up to the quota were allowed
PASS: the over-quota allocation was denied
PASS: denial surfaced as CUDA out-of-memory
PASS: workload observed the denial, not a crash
PASS: the guard released everything on free
PASS: probe exited cleanly
OVERALL: PASS (criterion 7 satisfied)
```

Points worth noting:

- `rc=2` is `CUDA_ERROR_OUT_OF_MEMORY`, so the workload sees an ordinary allocation failure and can handle it normally. It is not killed, and it continued to completion.
- The real allocator was never called for the denied request, so no VRAM was reserved and then thrown away.
- Freeing is tracked: usage returned to 0 and device free memory returned to its starting value.

**Still outstanding for this criterion:** the requirement also says *"while the other workload continues"*. The denial above was captured on a single workload. Criterion 6 below demonstrates two workloads running concurrently unaffected by each other, but a combined run — one job being denied while a second continues untouched — has not yet been captured as a single piece of evidence. That is the next test to add.

---

## Criterion 6 — GPU sharing with distinct quotas

> *Run two jobs with distinct VRAM quotas and capture timestamps proving overlapping CUDA progress on the same physical GPU.*

Setup: `job-a` quota **512 MiB** holding 384 MiB, `job-b` quota **1024 MiB** holding 768 MiB, both performing device memory traffic for 12 seconds.

Device telemetry while both ran — note 1314 MiB held and the GPU at 97–99%:

```text
  t+02s  0 MiB,    5920 MiB,  0 %
  t+04s  1314 MiB, 4607 MiB, 99 %
  t+06s  1314 MiB, 4607 MiB, 99 %
  t+08s  1314 MiB, 4607 MiB, 99 %
  t+10s  1314 MiB, 4607 MiB, 97 %
  t+12s  1314 MiB, 4607 MiB, 98 %
```

Interleaved timestamps from the two jobs, which is the proof of overlap:

```text
[07:33:47.112] [job-a] uuid=GPU-04280514-80a3-1dcf-1c5e-94e374365bef
[07:33:47.112] [job-b] uuid=GPU-04280514-80a3-1dcf-1c5e-94e374365bef
[07:33:47.116] [job-a] alloc   128 MiB  OK   total_held=384 MiB
[07:33:47.118] [job-b] alloc   128 MiB  OK   total_held=768 MiB
[07:33:48.252] [job-a] progress t+1s  copies=24  holding=384 MiB
[07:33:48.255] [job-b] progress t+1s  copies=25  holding=768 MiB
[07:33:53.257] [job-a] progress t+6s  copies=144 holding=384 MiB
[07:33:53.238] [job-b] progress t+6s  copies=146 holding=768 MiB
[07:33:59.272] [job-a] progress t+12s copies=288 holding=384 MiB
[07:33:59.270] [job-b] progress t+12s copies=292 holding=768 MiB
[07:33:59.282] [job-a] freed 3 chunks (384 MiB)
[07:33:59.285] [job-b] freed 6 chunks (768 MiB)
```

Overlap analysis and assertions:

```text
  job-a active window: 27227.111 -> 27239.330
  job-b active window: 27227.112 -> 27239.330
  overlapping duration: 12.218s

PASS: both jobs completed cleanly
PASS: both jobs ran on the same physical GPU
PASS: job-a was given its own quota (512 MiB)
PASS: job-b was given its own quota (1024 MiB)
PASS: job-a made CUDA progress
PASS: job-b made CUDA progress
PASS: their CUDA activity overlapped in time
PASS: neither job was denied (both stayed in quota)
OVERALL: PASS (criterion 6 satisfied)
```

Two jobs with different quotas made CUDA progress simultaneously for **12.218 seconds** on the same physical card, identified by UUID in both logs.

An honest note on what "CUDA progress" means here: the probe's activity phase performs repeated host-to-device memory copies with explicit context synchronisation, not kernel execution. Building a kernel needs `nvcc`, which needs the CUDA devel image — blocked by host disk space. The copies are genuine CUDA device work driven through the driver API, and the 99% utilisation confirms the device was busy, but it is memory traffic rather than compute. Worth replacing with a kernel once the demo workload image can be pulled.

---

## What this constrains in the design

1. **The guard's tally is authoritative.** Per-process VRAM is unavailable on WSL workers, so nothing external can audit a job's usage. The `CGPU_GUARD_STATUS_FILE` output is the per-job usage signal for A4's telemetry.
2. **A quota only binds dynamically linked workloads.** The stock `cuda-sample` image statically links the CUDA runtime and reaches the driver through `dlsym()`, so `LD_PRELOAD` cannot police it at all — it has zero undefined CUDA symbols. The scheduler must either restrict submissions to workloads whose linkage has been verified, or treat the quota as advisory and declare it. Detection signal: a workload that logs the guard's load line but no allocation lines is unpoliced. Full coverage table in [`pkg/guard/README.md`](../pkg/guard/README.md).
3. **Async pool allocations are not covered.** `cudaMallocAsync` and `cuMemAllocFromPoolAsync` bypass the guard. Whether PyTorch's caching allocator uses them is the open question blocking sign-off on the demo workload.
