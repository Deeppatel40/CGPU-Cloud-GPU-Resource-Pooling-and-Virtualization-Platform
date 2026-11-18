#!/usr/bin/env python3
"""CPU-only pipeline smoke workload (catalog id: cgpu-pipeline-smoke).

Does NOT use a GPU and does not train anything. It exists to verify the platform path
(API -> scheduler -> Kubernetes Job -> logs -> artifact upload -> download) on machines
without a GPU. Parameters come from /etc/cgpu/params.json like any CGPU workload.
"""
import json
import os
import sys
import time

params = {"steps": 5, "exit_code": 0, "simulate_oom": "no"}
try:
    with open("/etc/cgpu/params.json") as f:
        params.update(json.load(f))
except OSError:
    pass

out = "/outputs"
os.makedirs(os.path.join(out, "checkpoints"), exist_ok=True)
print("[SMOKE] job=%s quota=%s MiB params=%s" % (
    os.environ.get("CGPU_JOB_ID"), os.environ.get("CGPU_ALLOCATED_VRAM_MIB"), json.dumps(params)), flush=True)

start = time.time()
for step in range(1, int(params["steps"]) + 1):
    time.sleep(1)
    print("[SMOKE] step %d/%d" % (step, params["steps"]), flush=True)

if params.get("simulate_oom") == "yes":
    print("RuntimeError: CUDA out of memory (simulated by smoke test)", file=sys.stderr, flush=True)
    sys.exit(1)

payload = ("cgpu smoke checkpoint for %s\n" % os.environ.get("CGPU_JOB_ID")).encode()
with open(os.path.join(out, "checkpoints", "model_final.pt"), "wb") as f:
    f.write(payload)
with open(os.path.join(out, "metrics.json"), "w") as f:
    json.dump({"total_duration_seconds": round(time.time() - start, 2), "epochs_completed": int(params["steps"])}, f)
print("[SMOKE] wrote outputs; exiting with %s" % params["exit_code"], flush=True)
sys.exit(int(params["exit_code"]))
