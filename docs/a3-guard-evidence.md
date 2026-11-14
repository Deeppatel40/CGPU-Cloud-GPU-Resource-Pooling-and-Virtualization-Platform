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

Criterion 6 is additionally proven **through Kubernetes** — two Jobs submitted to the API
server, admitted, scheduled and run under the `nvidia` RuntimeClass. That run needs a
kubeconfig, which a k3s *agent* does not have, so it is driven from the control plane:

```bash
kubectl label node cgpu-gpu-worker-4050 cgpu.dev/gpu=true
test/e2e/gpu/k8s_concurrent_share_test.sh
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

### Upgraded: the same proof with real kernel execution

The run above is honest but weaker than it looks. Its activity phase was repeated host-to-device memory copies, not kernel execution — genuine CUDA device work, and 99% utilisation confirms the device was busy, but it demonstrates a shared *memory system* rather than shared *compute*. "Concurrent sharing" should mean the SMs.

The probe now executes a real kernel: a dependent chain of 20,000 fused multiply-adds per thread over 33.5 million threads, launched through `cuLaunchKernel` from PTX compiled once by `nvcc` and committed as [`cgpu_burn_ptx.h`](../test/e2e/gpu/cgpu_burn_ptx.h). Going through PTX and the driver API rather than linking the CUDA runtime is deliberate: it keeps the probe buildable with nothing but `gcc` on a node with no CUDA toolkit, and keeps it *dynamically* linked to libcuda, which is the property that makes it interposable at all. Linking cudart for `<<<>>>` syntax would have walked the probe straight into the static-linkage trap documented below.

The result is also verified rather than assumed. Each buffer is seeded to exactly `1.0f` and read back, so the log carries the arithmetic:

```text
[09:41:59.463] [job-neighbour] alloc   128 MiB  OK   total_held=512 MiB
[09:42:00.996] [job-neighbour] progress t+1s  kernels=8   element0=1.038147 holding=512 MiB
[09:42:04.906] [job-neighbour] progress t+5s  kernels=38  element0=1.181198 holding=512 MiB
[09:42:13.947] [job-neighbour] progress t+14s kernels=107 element0=1.510216 holding=512 MiB
[09:42:14.004] [job-neighbour] activity done mode=kernel launches=107 iters_per_thread=20000
                               threads=33554432 element0=1.510216 computed=yes
```

`computed=yes` means the device buffer no longer holds its seed value, so the kernel demonstrably *ran*; a launch that silently failed or never reached the device would leave `element0` at exactly 1.0. Re-running criterion 6 in this mode:

```text
  job-a active window: 35039.361 -> 35052.628
  job-b active window: 35039.360 -> 35052.621
  overlapping duration: 13.260s
OVERALL: PASS (criterion 6 satisfied - 13.260s of overlapping CUDA progress
          by two jobs with distinct quotas on GPU-04280514-80a3-1dcf-1c5e-94e374365bef)
```

**13.260 seconds of overlapping kernel execution**, two jobs, distinct quotas, one physical GPU.

One number in that run is more informative than the overlap itself. Alone, a job sustains ~7.6 kernel launches/second (38 in 5 s). With both jobs running, each sustains ~3.0 (36 in 12 s):

| | launches/s per job | aggregate |
| --- | --- | --- |
| one job alone | 7.6 | 7.6 |
| two jobs sharing | 3.0 | 6.0 |

Each job gets roughly half the throughput while the aggregate is largely preserved. That is the signature of **time-slicing**: the GPU is context-switching between two processes rather than running them side by side on separate SM partitions. It matters for the scheduler, because it means CGPU's sharing is fair-share in time, not spatial isolation — co-located jobs will slow each other down roughly in proportion to how many there are, and a latency-sensitive job cannot be protected by a VRAM quota alone. Hardware partitioning (MIG) is not available on a GeForce card, so this is the sharing model available to us, and the write-up should say so rather than implying stronger isolation than we have.

### Criterion 6 through Kubernetes

Both runs above drive containers through containerd directly, which deliberately takes
Kubernetes out of the picture so that a failure can only be the GPU, the runtime or the
guard. But the criterion is about the *platform*, so the proof also has to survive the
platform: admission, scheduling, the `nvidia` RuntimeClass and the pod sandbox.

That path is [`deploy/cgpu-criterion6-jobs.yaml`](../deploy/cgpu-criterion6-jobs.yaml) — two
Jobs, each with its own quota, driven by
[`test/e2e/gpu/k8s_concurrent_share_test.sh`](../test/e2e/gpu/k8s_concurrent_share_test.sh),
which additionally asserts that both pods landed on the same node and that each executed real
kernels (`computed=yes`).

The guard reaches the nodes via
[`deploy/cgpu-guard-installer.yaml`](../deploy/cgpu-guard-installer.yaml), a DaemonSet that
compiles it in place from a ConfigMap of this repo's sources using the CUDA devel image. That
answers Package 2's open question of how to ship the guard: the cluster has no registry, so
baking an image would mean a manual `ctr image import` on every node — exactly the per-node
step the DaemonSet removes. Generating the ConfigMap from the checkout at apply time also
means the deployed guard cannot drift from the source, and the in-place build doubles as a
safety check, because `build.sh` fails if the library would need a newer glibc than the
workload images carry. A node therefore gets either a loadable guard or none at all.

**Status: authored and validated, not yet executed.** The manifests parse, and their
contracts are asserted mechanically (RuntimeClass, `LD_PRELOAD`, quota variables, and the
deliberate absence of `PYTORCH_CUDA_ALLOC_CONF`). The run itself needs a kubeconfig with
apply rights, and `cgpu-gpu-worker-4050` is a k3s **agent** — it holds no admin credential,
by design. So this run belongs to whoever holds the control plane; it is handed over in
[COMMS.md](../COMMS.md) and needs one label first:

```bash
kubectl label node cgpu-gpu-worker-4050 cgpu.dev/gpu=true
test/e2e/gpu/k8s_concurrent_share_test.sh
```

---

## KPI — what the virtualisation layer costs

A quota mechanism that halves GPU throughput is a tax, not a sharing platform, so the cost
is a KPI in its own right. [`guard_overhead_test.sh`](../test/e2e/gpu/guard_overhead_test.sh)
measures two paths separately, because the guard's cost is not uniform:

- **the allocation path is interposed** — every allocation passes through a mutex, an
  addition and a bounds check before reaching the driver. Measured with 512 × 4 MiB
  allocations, the worst case, where per-call cost is amortised over the least work.
- **the compute path is not interposed** — kernel launches never touch the guard, so the
  expectation is no measurable difference. Testing a path where no overhead is predicted is
  what shows the measurement could detect overhead where it exists.

Each arm is the median of five runs, because a laptop GPU boosts and throttles and the first
CUDA process in a container pays context creation. The numbers include container start and
context creation, so they are an **upper bound** on the guard's share rather than an isolated
measure of it — which is the useful framing, since it is what a job actually experiences.

```text
==================== allocation path (512 x 4 MiB) ====================
  without guard: 3.127s   (samples: 4.280 3.011 2.566 3.312 3.127)
  with guard   : 3.188s   (samples: 3.188 3.285 3.248 3.053 3.131)

==================== compute path (10 s of kernels) ====================
  without guard: 13.353s  (samples: 13.108 13.353 13.356 13.380 12.814)
  with guard   : 13.350s  (samples: 13.350 13.429 13.350 13.555 12.969)
```

| Path | Interposed? | Without guard | With guard | Delta |
| --- | --- | --- | --- | --- |
| allocation (512 × 4 MiB) | yes, every call | 3.127 s | 3.188 s | **+2.0%** |
| compute (10 s of kernels) | no | 13.353 s | 13.350 s | **−0.0%** |

**The guard's cost is not scheduling-relevant.** +2.0% on the allocation-heavy arm is inside
run-to-run spread — note the 2.566 s and 4.280 s samples in the unguarded arm, a wider spread
than the difference between the two medians. An earlier run of the same test put the guarded
arm nominally *faster* (−1.5%), which is the honest way to read "no measurable cost" on a
boosting laptop GPU rather than claiming a precise figure the measurement cannot support.

The compute arm is the control, and it is the more informative of the two. Kernel launches
never pass through the guard, so a difference there would mean the measurement was picking up
something other than interposition. It came out at −0.0%, which says the method is sound and
that the +2.0% is attributable to the allocation path if it is real at all.

A note on why the test asserts loose bounds (25% and 15%) rather than tight ones: this is a
shared laptop host with a boosting GPU, and a tight threshold would produce a test that fails
randomly and then gets ignored, which is worse than no test. What must hold is that the guard
costs nothing near enough to change a placement decision — and at single-digit percent on the
allocation path only, it does not.

---

## What this constrains in the design

1. **The guard's tally is authoritative.** Per-process VRAM is unavailable on WSL workers, so nothing external can audit a job's usage. The `CGPU_GUARD_STATUS_FILE` output is the per-job usage signal for A4's telemetry.
2. **A quota only binds dynamically linked workloads.** The stock `cuda-sample` image statically links the CUDA runtime and reaches the driver through `dlsym()`, so `LD_PRELOAD` cannot police it at all — it has zero undefined CUDA symbols. The scheduler must either restrict submissions to workloads whose linkage has been verified, or treat the quota as advisory and declare it. Detection signal: a workload that logs the guard's load line but no allocation lines is unpoliced. Full coverage table in [`pkg/guard/README.md`](../pkg/guard/README.md).
3. **Async pool allocations are not covered.** `cudaMallocAsync` and `cuMemAllocFromPoolAsync` bypass the guard. Whether PyTorch's caching allocator uses them is the open question blocking sign-off on the demo workload.

---

## Criterion 7, complete form — denial with a neighbour unaffected

> *Verify an allocation error while the other workload continues.*

The earlier section proved the denial on a single workload. `quota_isolation_test.sh` captures both halves in one run: `job-neighbour` (quota 1024 MiB, holding 512 MiB) runs throughout while `job-victim` (quota 256 MiB) tries to take 1024 MiB. Combined demand 1536 MiB against 5920 MiB physically free.

```text
[07:46:18.523] [job-victim]    alloc 128 MiB OK       total_held=128 MiB
[07:46:18.525] [job-victim]    alloc 128 MiB OK       total_held=256 MiB
[07:46:18.525] [job-victim]    alloc 128 MiB REFUSED rc=2  total_held=256 MiB (out of memory)
[07:46:18.603] [job-victim]    RESULT=OK (denial observed as expected)

[07:46:18.899] [job-neighbour] progress t+3s  copies=141 holding=512 MiB
[07:46:23.893] [job-neighbour] progress t+8s  copies=379 holding=512 MiB
[07:46:29.893] [job-neighbour] progress t+14s copies=665 holding=512 MiB
[07:46:29.939] [job-neighbour] RESULT=OK
```

The neighbour produced **12 further progress reports after the victim's denial** and completed normally, holding its full 512 MiB throughout. All 8 assertions passed.

---

## Criterion 5 — coverage for the demo workload (PyTorch)

PyTorch is the demo workload, so whether its allocator is covered decides whether a quota is a real boundary. All three of its allocator strategies were run against a 512 MiB quota with a 1024 MiB request, on a card with 5920 MiB free.

| Strategy | Reaches the driver via | Result |
| --- | --- | --- |
| native (default) | `cudaMalloc`, linked dynamically | **CONTAINED** — refused, `torch.OutOfMemoryError` |
| `backend:cudaMallocAsync` | `cudaMallocAsync`, linked dynamically | **CONTAINED** — refused, `torch.OutOfMemoryError` |
| `expandable_segments:True` | `cuMemCreate`, resolved by name at runtime | **ESCAPED** — reserved 1040 MiB, 0 denials |

Capacity virtualisation works: in every case the workload reported `SEES total=512MiB free=512MiB` rather than the 6 GiB card, so a well-behaved allocator sizes itself to the quota instead of discovering the limit by failing.

**Mitigation for the escape, verified:** with `PYTORCH_CUDA_ALLOC_CONF` simply not passed through, the identical 1024 MiB request is refused (2 guard denials). The fix therefore belongs in the scheduler's Job mutator, which must strip or pin that variable. Without it, a job opts out of its quota with one environment variable.

### Four bugs found and fixed along the way

All are recorded because they are easy to reintroduce.

**Finished jobs were reported as live, holding VRAM.** Found while re-verifying the worker
agent against the guard. A status file written by a test run at 07:55 was still being
exported two hours later:

```text
cgpu_gpu_active_jobs{node="Deep"} 1
cgpu_job_vram_used_bytes{node="Deep",job_id="demo-job"} 268435456
```

Nothing ever removed the file, so every job that had ever run accumulated as a permanent
phantom: A4's dashboard would show VRAM in use that nobody holds, and any capacity
calculation reading these numbers would under-report free memory and refuse placements it
should accept. Fixed on both sides, because neither half is sufficient alone:

- the guard now `unlink()`s its own status file from a destructor on normal exit;
- the agent ignores a file whose mtime has stopped advancing (`-guard-status-stale`,
  default 2 minutes), because a job killed by `SIGKILL` or a container torn down mid-run
  never runs a destructor.

Verified: with a live job running and a two-hour-old file beside it, the agent reports only
the live job; after the live job exits its file is gone, and with only the stale file left
`cgpu_gpu_active_jobs` reads `0` with no per-job series at all.

**Double counting.** The driver implements the malloc-style APIs on top of the low-level ones, so a single `cuMemAlloc_v2` from a workload becomes `cuMemCreate` calls inside libcuda. With both interposed, each allocation was charged more than once: one 64 MiB request consumed 128 MiB of quota and the third request was wrongly refused. Fixed with a per-thread re-entrancy counter — only the outermost call accounts, nested driver-internal calls pass through already charged.

**The dlsym hook destabilises the driver.** Interposing `dlsym` closes the `expandable_segments` gap, but the driver resolves its own internals the same way, so the hook makes libcuda re-enter the guard. Bisected on hardware:

```text
  meminfo=1 dlsym=0 -> OK        2 allocations, RESULT=OK
  meminfo=0 dlsym=0 -> OK        2 allocations, RESULT=OK
  meminfo=* dlsym=1 -> SIGSEGV / hang
```

So `CGPU_GUARD_HOOK_DLSYM` defaults to off, and the `expandable_segments` gap is handled by environment sanitisation in the scheduler instead. Enabling the hook needs a way to tell driver-internal lookups from workload ones.

**CRLF line endings silently broke three e2e tests.** The repository had `core.autocrlf=true` on the Windows side and no `.gitattributes`, so git checked the shell scripts out with CRLF endings. On a Linux node every one of them then died at the shebang:

```text
  env: 'bash\r': No such file or directory
```

This mattered more than a cosmetic annoyance: the failures appeared in a regression run immediately after a change to the probe, so they looked exactly like a regression in the code under test. The real cause was a checkout artefact that depended on each developer's local git configuration, which means it could reproduce on one machine and not another. Fixed by committing a `.gitattributes` that pins LF for everything Linux executes (and CRLF for `.ps1`/`.bat`), then `git add --renormalize`. Worth knowing for the whole team, not just this node.

---

## Criterion 1 — worker agent: discovery, identity, health, allocatable VRAM

`cmd/cgpu-worker-agent` serves `/inventory`, `/healthz` and `/metrics`. One-shot output on this node:

```json
{
  "node_name": "cgpu-gpu-worker-4050",
  "healthy": true,
  "driver_version": "610.62",
  "reserve_mib": 512,
  "gpus": [{
    "uuid": "GPU-04280514-80a3-1dcf-1c5e-94e374365bef",
    "name": "NVIDIA GeForce RTX 4050 Laptop GPU",
    "pci_bus_id": "00000000:01:00.0",
    "memory_total_mib": 6141,
    "memory_free_mib": 5920,
    "allocatable_mib": 5629,
    "healthy": true
  }],
  "jobs": [{ "job_id": "demo-job", "used_bytes": 268435456, "quota_bytes": 536870912, "denials": 1 }]
}
```

`allocatable_mib` is total minus a reserve (default 512 MiB), **not** total minus used. Each CUDA context costs device memory no job's quota accounts for — measured, two jobs holding 1152 MiB showed 1314 MiB in use. Handing out the full total would let the scheduler overcommit the card, and a job would fail at startup instead of being queued. Reservation accounting itself stays in the scheduler.

Prometheus families for A4, with device-level and per-job memory kept separate on purpose because device usage cannot be attributed to jobs on WSL:

```text
cgpu_worker_healthy, cgpu_gpu_healthy
cgpu_gpu_memory_total_mib, cgpu_gpu_memory_allocatable_mib, cgpu_gpu_memory_used_mib
cgpu_gpu_utilization_percent, cgpu_gpu_temperature_celsius, cgpu_gpu_power_draw_watts
cgpu_gpu_active_jobs
cgpu_job_vram_used_bytes, cgpu_job_vram_peak_bytes, cgpu_job_vram_quota_bytes, cgpu_job_vram_denials_total
```

---

## Current test status on `cgpu-gpu-worker-4050`

| Test | Criterion | Result |
| --- | --- | --- |
| `guard_quota_test.sh` | 7 | PASS |
| `quota_isolation_test.sh` | 7 (complete) | PASS |
| `concurrent_share_test.sh` | 6 | PASS |
| `pytorch_coverage_test.sh` | 5 | PASS |
| `bypass_attempt_test.sh` | 5 | PASS, with the `expandable_segments` gap recorded and its mitigation verified |
| `guard_overhead_test.sh` | KPI | PASS |
| `k8s_concurrent_share_test.sh` | 6, via Kubernetes | Authored and validated; needs a control-plane kubeconfig to run |

Criteria 6 and 7 are both now proven with real kernel execution rather than memory traffic.

## Still open

- **`cuMemGetInfo` is not virtualised for the `expandable_segments` path**, because that path bypasses the wrappers entirely. Covered by the environment-sanitisation mitigation, not by the guard.
- **Approved-workload list.** Both limitations above point the same way: CGPU should only accept workloads whose linkage and allocator configuration have been verified. That is a scheduler decision (Package 2).
