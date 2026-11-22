# CGPU — Startup Guide

How to bring the platform up, machine by machine, and what to do when a piece
does not come up. For what the platform *is*, see [README.md](README.md); for
the measured GPU evidence, see [docs/a3-guard-evidence.md](docs/a3-guard-evidence.md).

---

## The short version

| You are on | Run this | You get |
| --- | --- | --- |
| **RTX 4050 laptop** (GPU worker) | `.\start-cgpu.ps1` | cluster join, guard, telemetry, Grafana, local portal |
| **PC1** (control plane) | `systemctl start k3s cgpu-scheduler` | K3s server + placement engine |
| **PC2** (API + portal) | `systemctl start mongod cgpu-api cgpu-portal` | REST API + the team's portal |

Start PC1 first, then PC2, then the GPU workers — each layer depends on the one
before it. A worker started early is not a problem, it simply retries.

---

## 1. GPU worker (the RTX 4050 laptop)

One command, from the repository root in PowerShell:

```powershell
.\start-cgpu.ps1
```

It is idempotent: anything already running is left alone, so re-run it freely.

```powershell
.\start-cgpu.ps1 -Status   # what is up, changes nothing
.\start-cgpu.ps1 -Load     # start, then put two matmul jobs on the GPU
.\start-cgpu.ps1 -Stop     # stop what it started (leaves k3s-agent up)
```

What it starts, and why each matters:

| Component | Port | Why it is needed |
| --- | --- | --- |
| `k3s-agent` | — | joins the cluster, so the scheduler can place jobs here |
| `/opt/cgpu/libcgpu_guard.so` | — | every GPU pod bind-mounts it; without it pods fail to start |
| `cgpu-worker-agent` | 9400 | the only source of GPU and per-job VRAM telemetry |
| Prometheus | 9090 | scrapes the agent and keeps history |
| Grafana | 3001 | the CGPU dashboard (`admin` / `admin`) — 3001 because 3000 is Next.js's default |
| portal | 3100 | local copy of the web portal |

Then open:

- Portal — <http://localhost:3100>
- Grafana — <http://localhost:3001>
- Prometheus — <http://localhost:9090> (Status → Targets should show all green)

The Linux half lives in [`scripts/cgpu-worker-stack.sh`](scripts/cgpu-worker-stack.sh)
and can be run on its own, which is also how a non-Windows GPU worker starts:

```bash
sudo ./scripts/cgpu-worker-stack.sh start|stop|status|load
```

### Make it survive a reboot

`start-cgpu.ps1` starts things for the current session. The WSL distro shuts
itself down when idle and takes K3s, the agent and both monitoring containers
with it — which looks exactly like a broken cluster. The script holds a WSL
session open to prevent that, but the durable fix is systemd:

```bash
wsl -d Ubuntu -u root
cp deploy/systemd/cgpu-*.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now cgpu-worker-agent cgpu-prometheus cgpu-grafana
```

After that only the portal needs starting by hand.

---

## 2. Control plane (PC1, `10.20.24.31`)

```bash
systemctl start k3s                 # Kubernetes server
systemctl start cgpu-scheduler      # placement engine, :8082
kubectl get nodes -o wide           # every worker should be Ready
```

### Give the scheduler the workers' real addresses

The built-in node list hardcodes each worker's IP. The GPU workers are laptops
on Wi-Fi, so their address changes on every rejoin, and a stale address makes
the portal show the wrong IP and telemetry fail permanently — the API probes
port 9400 at the recorded address. Refresh it from what Kubernetes reports:

```bash
scripts/gen-nodes-file.sh > /etc/cgpu/nodes.json
CGPU_NODES_FILE=/etc/cgpu/nodes.json   # then restart cgpu-scheduler
```

Re-run it after any laptop rejoins on a new address.

### Cluster-wide monitoring (optional)

```bash
kubectl apply -f deploy/monitoring/prometheus.yaml   # NodePort 30090
kubectl apply -f deploy/monitoring/grafana.yaml      # NodePort 30300
```

Pull the images first if the link is slow — they are ~400 MB together:

```bash
sudo k3s ctr images pull docker.io/prom/prometheus:v3.1.0
sudo k3s ctr images pull docker.io/grafana/grafana:11.5.1
```

---

## 3. API and team portal (PC2, `10.20.24.32`)

```bash
systemctl start mongod              # job store
systemctl start cgpu-api            # REST API, :8080
systemctl start cgpu-portal         # Next.js portal, :3000
curl -s http://10.20.24.32:8080/healthz
```

Team portal: <http://10.20.24.32:3000>

### Deploying a new API build

The API binary lives at `/home/g6/cgpu-app/bin/cgpu-api`. Keep a backup so the
swap is one `mv` to undo:

```bash
# build on any machine with Go; CGO_ENABLED=0 keeps it portable
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cgpu-api ./cmd/cgpu-api
scp cgpu-api g6@10.20.24.32:/home/g6/cgpu-app/bin/cgpu-api.new

# on PC2
cd /home/g6/cgpu-app/bin
cp -p cgpu-api cgpu-api.bak-$(date +%Y%m%d-%H%M%S)
mv cgpu-api.new cgpu-api && chmod 775 cgpu-api
sudo systemctl restart cgpu-api
```

The **workload catalog is served by the API**, so a workload added to the
repository only appears in the portal after this redeploy. The portal lists
such workloads marked *"not yet deployed"* and refuses to submit them, because
the API would reject them as unknown.

---

## 4. Running a job end to end

```
Portal → Submit job → "GPU matmul benchmark" → Submit
```

Expect: `PENDING` → `RUNNING / COMPUTING` → `SUCCEEDED`, the GPU at 100%, and
`metrics.json` plus the workload log available as artifacts.

Two workloads are approved:

| Workload | Image | Needs | Use it for |
| --- | --- | --- | --- |
| `gpu-matmul` | 407 MB | nothing | pooling, sharing, quotas — saturates the GPU in seconds |
| `pytorch-resnet18-cifar10` | 11.9 GB | CIFAR-10 (170 MB) | the training demo |

Prefer `gpu-matmul` unless the demo is specifically about training: it reaches a
busy GPU immediately and reports verifiable GFLOP/s, where the ResNet workload
needs a dataset whose upstream host serves at a few hundred bytes a second.

Each GPU node needs the image imported once — Docker and K3s have separate
image stores, so a `docker build` alone is not enough:

```bash
docker save cgpu/workloads/gpu-matmul:1.0.0 -o gpu-matmul.tar
sudo k3s ctr images import gpu-matmul.tar
sudo k3s ctr images ls | grep matmul
```

> Delete an existing copy first when rebuilding the same tag. Kubernetes only
> pulls when the image is missing, so it will keep running the old one:
> `sudo k3s ctr images rm docker.io/cgpu/workloads/<name>:1.0.0`

### Load without the portal

```powershell
.\start-cgpu.ps1 -Load
```

Two matmul jobs share the card under separate quotas and a third is denied
deliberately, so every dashboard panel has a live series.

---

## 5. When something does not come up

| Symptom | Cause and fix |
| --- | --- |
| Pod fails: `hostPath type check failed, /opt/cgpu is not a directory` | the guard is not installed — `.\start-cgpu.ps1` installs it |
| Job fails: `pull access denied` / stuck `Pulling image` | the image was never imported into K3s on that node (above) |
| Job fails: `CUDA is not available` | NVIDIA runtime broken on that node — see `docs/gpu-worker-4050.md` §3 |
| Portal: `HTTP 500 … API unreachable` | `cgpu-api` is down on PC2, or `CGPU_API_URL` was baked at build time pointing elsewhere |
| Portal shows the wrong API in its footer | `CGPU_API_URL` is read at **build** time; set it and rebuild, do not just restart |
| Everything on the worker is suddenly down | the WSL distro idled out — re-run the script, then install the systemd units |
| Telemetry says `AGENT_UNREACHABLE` | the scheduler has a stale IP for that worker — regenerate `nodes.json` (§2) |
| Grafana/portal panels read "unavailable" | the worker agent is not running, or Prometheus has no targets up |
| A workload is missing from Submit | the API has not been redeployed with it (§3) |
| Docker build dies mid-extract with an RPC EOF | the Docker data disk is out of space; it must live on `D:` on this laptop |

Logs, in the places that actually answer the question:

```bash
# worker (inside WSL)
tail -f /var/log/cgpu-agent.log /var/log/cgpu-prometheus.log /var/log/cgpu-grafana.log
journalctl -u k3s-agent -f

# PC1 / PC2
journalctl -u cgpu-scheduler -f
journalctl -u cgpu-api -f
```

---

## 6. Verifying the GPU side

All of these run on a GPU worker and need the guard and probe built
(`pkg/guard/build.sh`, `test/e2e/gpu/build.sh`):

```bash
sudo test/e2e/gpu/guard_quota_test.sh         # an over-quota allocation is refused
sudo test/e2e/gpu/quota_isolation_test.sh     # one job denied while a neighbour runs on
sudo test/e2e/gpu/concurrent_share_test.sh    # two jobs, distinct quotas, overlapping kernels
sudo test/e2e/gpu/pytorch_coverage_test.sh    # coverage for the PyTorch allocator
sudo test/e2e/gpu/bypass_attempt_test.sh      # bypass attempts and the mitigation
sudo test/e2e/gpu/guard_overhead_test.sh      # what interposition costs
```

Note the host paths these expect are on `D:` on this laptop: `C:` has no space,
and a full `C:` has already taken the WSL VM down mid-run once.
