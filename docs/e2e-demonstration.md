# End-to-end demonstration (Issue B4 / #14)

From a browser on a CPU-only machine to remote GPU execution and checkpoint download, with
evidence recorded automatically by `scripts/e2e_demo.py`.

## Status

| Part | State |
|---|---|
| Go API + MongoDB on Ubuntu PC 2 (`cgpu-api`, :8080) | Deployed, systemd |
| Next.js portal on Ubuntu PC 2 (`cgpu-portal`, :3000) | Deployed, systemd |
| Scheduler on PC 1 (`cgpu-scheduler`, :8082) | Deployed; persists state, follows node readiness |
| Full pipeline (submit → place → K8s Job → logs → artifact upload → download → release) | **Verified on the cluster without GPUs** using the CPU-only `cgpu-pipeline-smoke` workload on `cgpu-cpu-worker` (success, queueing, non-zero exit, OOM classification, cancel, SSE logs, SHA-256 checked download) |
| Real GPU run (two jobs on one card, queueing, trained checkpoint) | **Pending: needs the RTX 4050 / 3050 laptops online.** Run the steps below and commit the generated evidence file. |

## Architecture of the demo path

```
Browser (any LAN PC) ──► Portal :3000 (PC 2) ──proxy──► CGPU API :8080 (PC 2) ──► MongoDB (PC 2, localhost)
                                                          │  POST /api/v1/schedule, /release
                                                          ├──────────────────────────► Scheduler :8082 (PC 1)
                                                          │  kubectl as ServiceAccount cgpu-api (namespace cgpu-jobs only)
                                                          └──► K3s API (PC 1) ──► Job pod on the assigned GPU worker
                                                                                   │  LD_PRELOAD libcgpu_guard.so, quota = reservation
                                                          ◄── PUT /internal/v1/jobs/{id}/artifacts (per-job token) ──┘
```

## One-time preparation of each GPU laptop

1. **Keep the node Ready**: WSL session open (keep-alive task from `docs/gpu-worker-4050.md`) and
   `sudo systemctl status k3s-agent` active. `kubectl get nodes` on PC 1 must show it `Ready`; the
   scheduler marks it ONLINE within 30 s.
2. **Guard library** at `/opt/cgpu/libcgpu_guard.so` (`pkg/guard/build.sh`, then
   `sudo install -D -m 644 libcgpu_guard.so /opt/cgpu/libcgpu_guard.so`). The Job template mounts
   `/opt/cgpu` with `type: Directory`, so a missing directory fails the pod instead of running unguarded.
3. **Workload image** in the node's containerd (there is no registry; `imagePullPolicy: IfNotPresent`):
   ```bash
   cd workloads/demonstration
   docker build -t cgpu/workloads/pytorch-resnet18-cifar10:1.0.0 .
   docker save cgpu/workloads/pytorch-resnet18-cifar10:1.0.0 | sudo k3s ctr images import -
   sudo k3s ctr images ls | grep resnet18
   ```
   (Without Docker in WSL, build on any machine with Docker and copy the `docker save` tarball.)
4. **Dataset cache** (optional but recommended — the CIFAR-10 host is slow): jobs use
   `/var/lib/cgpu/datasets` on the node as `/data`. Pre-seed it once:
   `sudo mkdir -p /var/lib/cgpu/datasets/cifar10 && sudo tar -C /var/lib/cgpu/datasets/cifar10 -xzf cifar-10-python.tar.gz`.
5. **Telemetry** (optional): run `cgpu-worker-agent` (port 9400). Without it the portal shows
   measured usage as *unavailable*, never as zero.

## Running the demonstration

1. Open `http://10.20.24.32:3000` from a CPU-only machine (e.g. PC 1 or the Mac).
   - *GPU pool* page: both laptops ONLINE, capacity / reserved / free per GPU.
2. Submit two **ResNet-18** jobs from the *Submit* page with **1536 MiB** each, `epochs=2`,
   `max_steps_per_epoch=300`. Best-fit packs both onto the same card (the RTX 3050 if both
   laptops are up: 3584 MiB allocatable). Open both job pages: same GPU UUID, both RUNNING,
   live logs scrolling with timestamps.
3. Or run everything unattended and record evidence:
   ```bash
   python3 scripts/e2e_demo.py --api http://10.20.24.32:8080            # ~15-30 min
   python3 scripts/e2e_demo.py --api http://10.20.24.32:8080 --baseline # + whole-GPU comparison
   ```
   It writes `docs/evidence/e2e-<UTC time>.md` containing:
   - portal job IDs, quotas, physical GPU UUIDs, scheduled/started/completed timestamps;
   - the overlapping RUNNING interval and the number of training-step log lines from each job
     inside it (overlapping CUDA progress on one card);
   - a queued job: fillers take the free VRAM, a 1536 MiB job is accepted as
     `PENDING / WAITING_FOR_CAPACITY` and is scheduled only after a filler releases its slice;
   - `model_final.pt` downloaded through the API, SHA-256 verified, and loaded into ResNet-18
     (when torch is installed where the script runs);
   - with `--baseline`: the same two jobs run one after another with a whole-GPU slice each, and
     both wall-clock times, with the conditions stated.
4. Quota enforcement: run `bash test/e2e/gpu/guard_quota_test.sh` on a laptop and point to
   `docs/a3-guard-evidence.md` (criteria 5-7).

## Workload and allocation paths verified for quota enforcement

From `docs/a3-guard-evidence.md`, on the RTX 4050:
- Runtime API `cudaMalloc`/`cudaFree`, driver API `cuMemAlloc_v2`/`cuMemFree_v2`, and
  `cudaMemGetInfo`/`cuMemGetInfo_v2` (capacity virtualised to the quota) — enforced.
- PyTorch native allocator and `backend:cudaMallocAsync` — contained (`torch.OutOfMemoryError`).
- PyTorch `expandable_segments` (`cuMemCreate`) — **escapes** the guard. Mitigation in this
  platform: users can only pick catalog workloads and parameters (no environment variables), and
  the shared Job template pins `PYTORCH_CUDA_ALLOC_CONF=backend:native` for every GPU job.
- Statically linked CUDA binaries are not policed by `LD_PRELOAD`; only catalog workloads whose
  linkage was verified (the PyTorch image) are offered.

## WSL limitations to state in the demo

- A GPU laptop is only a node while a WSL session is open; closing it makes the node NotReady.
  CGPU marks it OFFLINE within 30 s and queues new jobs instead of placing them there.
- No pod overlay networking on WSL workers (kernel VXLAN sockets are not mirrored), so workload
  pods use `hostNetwork: true` and reach the API at the PC's LAN address.
- The driver reports no per-process VRAM under WSL (`used_memory` is `[N/A]`); per-job usage comes
  from the guard's status files via `cgpu-worker-agent`.
- No hardware partitioning (MIG/vGPU) on consumer GPUs: compute is time-shared, so two jobs on one
  card each run slower than alone; memory isolation comes from the guard.
- The CUDA context (~300-500 MiB per process) is outside every quota; the scheduler reserves
  512 MiB per GPU for it, and packing more processes increases that overhead.
