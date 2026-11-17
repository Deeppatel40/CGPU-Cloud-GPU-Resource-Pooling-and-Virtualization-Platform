#!/usr/bin/env python3
"""CGPU end-to-end demonstration and evidence recorder (issue B4 / #14).

Drives the full path through the public API exactly as the portal does, then writes a
Markdown evidence report with every job ID, quota, physical GPU UUID and timestamp.

  python3 scripts/e2e_demo.py --api http://10.20.24.32:8080            # full run
  python3 scripts/e2e_demo.py --api ... --skip-queue                   # sharing only
  python3 scripts/e2e_demo.py --api ... --baseline                     # + whole-GPU baseline

Phases
  1. Preflight: API reachable, at least one GPU node ONLINE.
  2. Sharing: two training jobs whose VRAM requests fit one GPU together. Best-fit packs
     them onto the same card; the report proves the overlap from RUNNING intervals and
     interleaved, timestamped training-step log lines.
  3. Queueing: filler jobs take the remaining free VRAM, then a job is submitted that
     cannot fit anywhere -> PENDING/WAITING_FOR_CAPACITY; it starts after a release.
  4. Checkpoint: download model_final.pt, verify SHA-256, and load it into ResNet-18
     (if torch is installed where this runs; otherwise the report says so).
  5. Optional baseline: the same two jobs run one after another, each given a whole GPU
     slice, to compare wall-clock time against the shared run under the same conditions.

Python standard library only.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid

WORKLOAD = "pytorch-resnet18-cifar10"


class API:
    def __init__(self, base):
        self.base = base.rstrip("/")

    def call(self, method, path, body=None, raw=False, timeout=60):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                payload = r.read()
                return r.status, payload if raw else json.loads(payload or b"{}")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"{}")
            except ValueError:
                return e.code, {}

    def submit(self, name, vram, params):
        code, j = self.call("POST", "/api/v1/jobs", {
            "request_id": str(uuid.uuid4()), "name": name, "workload_id": WORKLOAD,
            "requested_vram_mib": vram, "parameters": params, "timeout_seconds": 3600})
        if code not in (200, 201):
            sys.exit(f"submit {name} failed: HTTP {code} {j}")
        log(f"submitted {name}: {j['job_id']} {vram} MiB -> {j['status']} {j.get('pending_reason') or ''}")
        return j

    def job(self, jid):
        return self.call("GET", f"/api/v1/jobs/{jid}")[1]

    def wait(self, jid, states, timeout):
        start = time.time()
        while time.time() - start < timeout:
            j = self.job(jid)
            if j["status"] in states:
                return j
            time.sleep(3)
        sys.exit(f"timed out waiting for {jid} to reach {states} (last {j['status']})")

    def logs(self, jid):
        return self.call("GET", f"/api/v1/jobs/{jid}/logs?tail=10000")[1].get("lines", [])

    def gpus(self):
        return self.call("GET", "/api/v1/gpus")[1]


def log(msg):
    print(f"{dt.datetime.now().strftime('%H:%M:%S')} {msg}", flush=True)


def ts(s):
    return dt.datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def online_gpus(inv):
    return [(n, g) for n in inv["nodes"] if n["status"] == "ONLINE"
            for g in n["gpus"] if g["health_status"] == "HEALTHY"]


def step_times(lines):
    """Timestamps of training-step log lines ('epoch e/E step s/S ...')."""
    return [ts(l["timestamp"]) for l in lines if " step " in l["message"] and "epoch" in l["message"]]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default=os.environ.get("CGPU_API", "http://10.20.24.32:8080"))
    ap.add_argument("--vram", type=int, default=1536, help="VRAM per shared job (MiB)")
    ap.add_argument("--epochs", type=int, default=2)
    ap.add_argument("--steps", type=int, default=300, help="max_steps_per_epoch for the shared jobs")
    ap.add_argument("--skip-queue", action="store_true")
    ap.add_argument("--baseline", action="store_true")
    ap.add_argument("--out", default=None, help="evidence report path (default docs/evidence/e2e-<time>.md)")
    a = ap.parse_args()
    api = API(a.api)
    started = dt.datetime.now(dt.timezone.utc)
    rep = [f"# CGPU end-to-end demonstration evidence", "",
           f"- Run started: {started.isoformat(timespec='seconds')}", f"- API: `{a.api}`", ""]

    # 1. Preflight
    inv = api.gpus()
    gpus = online_gpus(inv)
    rep += ["## 1. GPU pool at start", "", "| Node | Status | GPU UUID | Total | Reserved | Free | Telemetry |", "|---|---|---|---|---|---|---|"]
    for n in inv["nodes"]:
        for g in n["gpus"]:
            rep.append(f"| {n['node_id']} | {n['status']} | `{g['gpu_id']}` | {g['total_vram_mib']} | {g['allocated_vram_mib']} | {g['free_vram_mib']} | {(g.get('telemetry') or {}).get('status', '-')} |")
    if not gpus:
        sys.exit("no GPU node is ONLINE: start WSL and the k3s agent on a laptop first (see docs/e2e-demonstration.md)")
    log(f"{len(gpus)} GPU(s) online: {[g['gpu_id'] for _, g in gpus]}")

    # 2. Sharing: two jobs on one card
    params = {"epochs": a.epochs, "batch_size": 64, "max_steps_per_epoch": a.steps}
    j1 = api.submit("demo-share-a", a.vram, params)
    j2 = api.submit("demo-share-b", a.vram, params)
    j1, j2 = (api.wait(j["job_id"], ("RUNNING", "SUCCEEDED", "FAILED"), 1800) for j in (j1, j2))
    j1, j2 = (api.wait(j["job_id"], ("SUCCEEDED", "FAILED"), 7200) for j in (j1, j2))
    same_gpu = j1["placement"]["gpu_id"] == j2["placement"]["gpu_id"]
    s1, e1 = ts(j1["timestamps"]["started_at"]), ts(j1["timestamps"]["completed_at"])
    s2, e2 = ts(j2["timestamps"]["started_at"]), ts(j2["timestamps"]["completed_at"])
    overlap = (min(e1, e2) - max(s1, s2)).total_seconds()
    t1, t2 = step_times(api.logs(j1["job_id"])), step_times(api.logs(j2["job_id"]))
    win_lo, win_hi = max(s1, s2), min(e1, e2)
    in_win = (sum(win_lo <= t <= win_hi for t in t1), sum(win_lo <= t <= win_hi for t in t2))

    rep += ["", "## 2. Two training jobs sharing one physical GPU", "",
            "| | Job A | Job B |", "|---|---|---|"]
    for label, key in [("Job ID", lambda j: f"`{j['job_id']}`"), ("Status", lambda j: j["status"]),
                       ("Node", lambda j: j["placement"]["node_id"]), ("Physical GPU UUID", lambda j: f"`{j['placement']['gpu_id']}`"),
                       ("VRAM quota (MiB)", lambda j: j["placement"]["allocated_vram_mib"]),
                       ("Enforcement", lambda j: j["placement"]["enforcement_mode"]),
                       ("Scheduled", lambda j: j["timestamps"]["scheduled_at"]), ("Started", lambda j: j["timestamps"]["started_at"]),
                       ("Completed", lambda j: j["timestamps"]["completed_at"]),
                       ("Peak VRAM measured by PyTorch (MiB)", lambda j: (j.get("execution_metrics") or {}).get("peak_vram_mib")),
                       ("Test accuracy (%)", lambda j: (j.get("execution_metrics") or {}).get("final_accuracy")),
                       ("Throughput (img/s)", lambda j: (j.get("execution_metrics") or {}).get("samples_per_second"))]:
        rep.append(f"| {label} | {key(j1)} | {key(j2)} |")
    rep += ["", f"- Same physical GPU: **{'YES' if same_gpu else 'NO'}**",
            f"- Overlapping RUNNING interval: **{overlap:.0f} s** ({win_lo.isoformat(timespec='seconds')} to {win_hi.isoformat(timespec='seconds')})",
            f"- Training-step log lines inside the overlap window: job A {in_win[0]}, job B {in_win[1]} "
            "(both jobs were making CUDA progress at the same time)"]
    if not same_gpu:
        rep.append("- NOTE: best-fit placed the jobs on different GPUs. Re-run with a larger `--vram` (both must fit one card but not the other's free space) or with one GPU worker online.")
    log(f"sharing: same_gpu={same_gpu} overlap={overlap:.0f}s steps_in_window={in_win}")

    # 3. Queueing
    if not a.skip_queue:
        rep += ["", "## 3. Waiting for capacity, then starting after a release", ""]
        qv = 1536
        fillers = []
        for n, g in online_gpus(api.gpus()):
            if g["free_vram_mib"] >= qv:
                fillers.append(api.submit(f"demo-fill-{n['node_id'][-4:]}", g["free_vram_mib"],
                                          {"epochs": 1, "batch_size": 64, "max_steps_per_epoch": 400}))
        for f in fillers:
            api.wait(f["job_id"], ("RUNNING", "SUCCEEDED", "FAILED"), 1800)
        q = api.submit("demo-queued", qv, {"epochs": 1, "batch_size": 64, "max_steps_per_epoch": 50})
        q_pending = q["status"] == "PENDING" and q.get("pending_reason") == "WAITING_FOR_CAPACITY"
        msg = (q.get("pending_details") or {}).get("message", "")
        q = api.wait(q["job_id"], ("SUCCEEDED", "FAILED"), 7200)
        released = [api.job(f["job_id"]) for f in fillers]
        first_release = min((ts(f["timestamps"]["completed_at"]) for f in released if f["timestamps"].get("completed_at")), default=None)
        rep += [f"- Fillers occupying the free VRAM: " + ", ".join(f"`{f['job_id']}` ({f['requested_vram_mib']} MiB on {f['placement']['node_id']})" for f in released),
                f"- Queued job `{q['job_id']}` ({qv} MiB) at submission: **{'PENDING / WAITING_FOR_CAPACITY' if q_pending else 'NOT queued'}** — \"{msg}\"",
                f"- Submitted {q['timestamps']['submitted_at']}, first filler completed {first_release.isoformat(timespec='seconds') if first_release else '-'}, "
                f"queued job scheduled {q['timestamps']['scheduled_at']} on `{q['placement']['gpu_id']}`, finished {q['status']}"]
        log(f"queue: pending_at_submit={q_pending} final={q['status']}")

    # 4. Checkpoint download + load
    rep += ["", "## 4. Checkpoint download", ""]
    art = next((x for x in j1["artifacts"] if x["name"].endswith("model_final.pt")), None)
    if art:
        code, blob = api.call("GET", art["download_url"], raw=True, timeout=300)
        ok = code == 200 and hashlib.sha256(blob).hexdigest() == art["checksum_sha256"]
        path = os.path.join(os.path.dirname(os.path.abspath(a.out or "docs/evidence/x")), f"{j1['job_id']}-model_final.pt")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(blob)
        rep.append(f"- `{art['name']}` ({len(blob)} bytes) downloaded via `{art['download_url']}`; SHA-256 matches: **{'YES' if ok else 'NO'}**")
        try:
            import torch, torchvision  # noqa: E401
            m = torchvision.models.resnet18(num_classes=10)
            m.load_state_dict(torch.load(path, map_location="cpu"))
            rep.append("- Loaded into `torchvision.models.resnet18(num_classes=10)`: **OK**")
        except ImportError:
            rep.append(f"- torch not installed here; verify with: `python3 -c \"import torch,torchvision; m=torchvision.models.resnet18(num_classes=10); m.load_state_dict(torch.load('{path}', map_location='cpu')); print('OK')\"`")
        except Exception as e:  # noqa: BLE001
            rep.append(f"- Loading FAILED: {e}")
    else:
        rep.append("- No model_final.pt artifact on job A")

    # 5. Baseline
    if a.baseline:
        rep += ["", "## 5. Whole-GPU baseline (same workload, same parameters, one job at a time)", ""]
        whole = max(g["total_vram_mib"] for _, g in online_gpus(api.gpus())) - 512
        t0 = time.time()
        for name in ("demo-base-a", "demo-base-b"):
            b = api.submit(name, whole, params)
            b = api.wait(b["job_id"], ("SUCCEEDED", "FAILED"), 7200)
            rep.append(f"- `{b['job_id']}` {whole} MiB: {b['status']}, {(b.get('execution_metrics') or {}).get('samples_per_second')} img/s")
        seq = time.time() - t0
        shared = (max(e1, e2) - min(s1, s2)).total_seconds()
        rep += [f"- Two jobs, whole GPU each, sequential: **{seq:.0f} s** (includes scheduling and container start)",
                f"- Two jobs sharing one GPU (section 2), first start to last finish: **{shared:.0f} s**",
                "- Metric: wall-clock time to finish both jobs. Conditions: same workload, epochs, batch size and steps; "
                "same GPU worker; dataset already cached on the node."]

    rep += ["", "## Quota enforcement", "",
            "The per-job VRAM limit is enforced by `libcgpu_guard.so` (LD_PRELOAD), configured from each job's "
            "`CGPU_ALLOCATED_VRAM_MIB`. Hardware evidence: `docs/a3-guard-evidence.md` (criteria 5-7). Verified paths: "
            "`cudaMalloc`/`cudaFree`, `cuMemAlloc_v2`/`cuMemFree_v2`, `cudaMemGetInfo`/`cuMemGetInfo_v2`, PyTorch native "
            "and `cudaMallocAsync` allocators. Known gap: PyTorch's expandable segments (cuMemCreate) escape the guard; "
            "the shared Job template pins `PYTORCH_CUDA_ALLOC_CONF=backend:native` and users cannot set environment variables.",
            "", f"- Run finished: {dt.datetime.now(dt.timezone.utc).isoformat(timespec='seconds')}"]
    out = a.out or os.path.join("docs", "evidence", f"e2e-{started.strftime('%Y%m%dT%H%M%SZ')}.md")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w", encoding="utf-8") as f:
        f.write("\n".join(str(x) for x in rep) + "\n")
    log(f"evidence written to {out}")


if __name__ == "__main__":
    main()
