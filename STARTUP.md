# CGPU — Startup Guide

How to bring the platform up, machine by machine, and what to do when a piece
does not come up. For what the platform *is*, see [README.md](README.md); for
the measured GPU evidence, see [docs/a3-guard-evidence.md](docs/a3-guard-evidence.md).

---

## The cluster

| Machine | Address | Role |
| --- | --- | --- |
| `cgpu-control-plane` (PC1) | `10.20.24.31` | K3s server, **scheduler / placement engine** (:8082), cluster Prometheus + Grafana |
| `cgpu-cpu-worker` (PC2) | `10.20.24.32` | **the server**: REST API (:8080), MongoDB, the team's portal (:3000) |
| `cgpu-gpu-worker-4050` | DHCP, Wi-Fi | GPU worker — RTX 4050, 6141 MiB |
| `cgpu-gpu-worker-3050` | DHCP, Wi-Fi | GPU worker — RTX 3050, 4096 MiB |

The two PCs sit on the wired `10.20.24.0/24` network and the GPU laptops join
over Wi-Fi on `10.1.144.0/22`. Workers only need **outbound** access to PC1:6443
to join, which is why a laptop on the campus Wi-Fi works as a node at all.

### Anyone on the university network

No VPN, no cluster access, nothing installed — just a browser:

| What | Where |
| --- | --- |
| **Submit jobs, watch logs, see the GPU pool** | <http://10.20.24.32:3000> |
| REST API | <http://10.20.24.32:8080> |
| Grafana dashboards | <http://10.20.24.31:30300> (`admin` / `admin`) |
| Prometheus | <http://10.20.24.31:30090> |

## The short version

| You are on | Run this | You get |
| --- | --- | --- |
| **A GPU laptop** (worker) | `.\start-cgpu.ps1` | cluster join, guard, telemetry, local Grafana + portal |
| **PC1** (control plane) | `systemctl start k3s cgpu-scheduler` | K3s server + placement engine |
| **PC2** (the server) | `systemctl start mongod cgpu-api cgpu-portal` | REST API + the team's portal |

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

## 1b. The same thing by hand (no scripts)

Every command `start-cgpu.ps1` runs, in order, in case you want to start pieces
selectively, run them on a worker without the scripts, or see what the script
is actually doing. Run the Linux commands as root inside the distro:

```powershell
wsl -d Ubuntu -u root
```

Paths below assume the repo at `/mnt/d/Cloud_Project/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform`
(`export REPO=` that to shorten them).

### 1. Cluster agent

```bash
systemctl start k3s-agent
systemctl is-active k3s-agent
nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
```

### 2. Allocation guard

Every GPU pod bind-mounts `/opt/cgpu`; without the library they fail to start.

```bash
cd $REPO/pkg/guard
./build.sh /var/lib/cgpu/bin
install -D -m 644 /var/lib/cgpu/bin/libcgpu_guard.so /opt/cgpu/libcgpu_guard.so
```

### 3. Worker agent (:9400)

```bash
# only if the binary is missing (needs Go)
cd $REPO && go build -buildvcs=false -o /usr/local/bin/cgpu-worker-agent ./cmd/cgpu-worker-agent

mkdir -p /var/run/cgpu/guard
setsid /usr/local/bin/cgpu-worker-agent \
    -listen 0.0.0.0:9400 \
    -guard-status-dir /var/run/cgpu/guard \
    -reserve-mib 512 >/var/log/cgpu-agent.log 2>&1 < /dev/null &

curl -s http://127.0.0.1:9400/healthz
```

`setsid` matters: without it the agent dies with the shell that started it.

### 4. Monitoring config (first time only)

Prometheus and Grafana read these from `/var/lib/cgpu/monitoring`. The cluster
deployment in [`deploy/monitoring/`](deploy/monitoring/) carries the same
scrape config and dashboard; these are the node-local equivalents.

```bash
mkdir -p /var/lib/cgpu/monitoring/{prometheus,prom-data,grafana-data} \
         /var/lib/cgpu/monitoring/grafana/{dashboards,provisioning/datasources,provisioning/dashboards}

cat > /var/lib/cgpu/monitoring/prometheus/prometheus.yml <<'YAML'
global:
  scrape_interval: 10s
scrape_configs:
  # Scraped over loopback on purpose: this sidesteps the control plane's
  # hardcoded node IP, so telemetry keeps working when that address is stale.
  - job_name: cgpu-worker-agent
    scrape_timeout: 5s
    static_configs:
      - targets: ["127.0.0.1:9400"]
        labels: { node: cgpu-gpu-worker-4050 }
  - job_name: cgpu-scheduler
    scrape_timeout: 5s
    static_configs:
      - targets: ["10.20.24.31:8082"]
        labels: { component: scheduler }
  - job_name: prometheus
    static_configs:
      - targets: ["127.0.0.1:9090"]
YAML

cat > /var/lib/cgpu/monitoring/grafana/provisioning/datasources/datasources.yaml <<'YAML'
apiVersion: 1
datasources:
  - name: CGPU-Prometheus
    type: prometheus
    access: proxy
    url: http://127.0.0.1:9090
    isDefault: true
YAML

cat > /var/lib/cgpu/monitoring/grafana/provisioning/dashboards/provider.yaml <<'YAML'
apiVersion: 1
providers:
  - name: cgpu
    folder: CGPU
    type: file
    allowUiUpdates: true
    options:
      path: /var/lib/grafana/dashboards
YAML

# the dashboard itself, lifted out of the cluster manifest so both show the
# same panels
python3 - <<'PY'
import json, yaml, os
src = os.path.expandvars("$REPO/deploy/monitoring/grafana.yaml")
for d in yaml.safe_load_all(open(src)):
    if d and d.get("kind") == "ConfigMap" and "cgpu-overview.json" in (d.get("data") or {}):
        out = "/var/lib/cgpu/monitoring/grafana/dashboards/cgpu-overview.json"
        json.dump(json.loads(d["data"]["cgpu-overview.json"]), open(out, "w"), indent=2)
        print("wrote", out)
PY

chown -R 472:472 /var/lib/cgpu/monitoring/grafana-data   # grafana runs as uid 472
```

### 5. Prometheus (:9090)

```bash
k3s ctr images pull docker.io/prom/prometheus:v3.1.0      # first time only
k3s ctr container rm cgpu-prometheus 2>/dev/null          # clear any corpse

setsid k3s ctr run --rm --net-host \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/prometheus,dst=/etc/prometheus,options=rbind:ro" \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/prom-data,dst=/prometheus,options=rbind:rw" \
    docker.io/prom/prometheus:v3.1.0 cgpu-prometheus \
    /bin/prometheus --config.file=/etc/prometheus/prometheus.yml \
        --storage.tsdb.path=/prometheus \
        --storage.tsdb.retention.time=7d \
        --web.listen-address=0.0.0.0:9090 >/var/log/cgpu-prometheus.log 2>&1 < /dev/null &

curl -s http://127.0.0.1:9090/-/ready
```

### 6. Grafana (:3001)

```bash
k3s ctr images pull docker.io/grafana/grafana:11.5.1      # first time only
k3s ctr container rm cgpu-grafana 2>/dev/null

setsid k3s ctr run --rm --net-host \
    --env GF_SECURITY_ADMIN_USER=admin \
    --env GF_SECURITY_ADMIN_PASSWORD=admin \
    --env GF_USERS_ALLOW_SIGN_UP=false \
    --env GF_SERVER_HTTP_PORT=3001 \
    --env GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/cgpu-overview.json \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/grafana-data,dst=/var/lib/grafana,options=rbind:rw" \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/grafana/dashboards,dst=/var/lib/grafana/dashboards,options=rbind:ro" \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/grafana/provisioning,dst=/etc/grafana/provisioning,options=rbind:ro" \
    docker.io/grafana/grafana:11.5.1 cgpu-grafana >/var/log/cgpu-grafana.log 2>&1 < /dev/null &

curl -s http://127.0.0.1:3001/api/health
```

`GF_SERVER_HTTP_PORT=3001` is required, not cosmetic: 3000 is also Next.js's
default port, and under WSL mirrored networking the distro shares the Windows
ports — so any `npm start` in `portal/` makes Grafana fail with *"address
already in use"* while nothing appears to be listening inside WSL.

If `admin / admin` is rejected, the password was fixed when Grafana's database
was first created and the env var no longer applies. Reset it:

```bash
k3s ctr task kill -s SIGTERM cgpu-grafana; sleep 4; k3s ctr container rm cgpu-grafana
k3s ctr run --rm \
    --mount "type=bind,src=/var/lib/cgpu/monitoring/grafana-data,dst=/var/lib/grafana,options=rbind:rw" \
    docker.io/grafana/grafana:11.5.1 cgpu-grafana-reset \
    grafana cli --homepath /usr/share/grafana admin reset-admin-password admin
```

### 7. Portal (:3100) — Windows side, not WSL

```powershell
cd portal
npm install                                  # first time only

# CGPU_API_URL is read at BUILD time: Next.js bakes the /api/v1 rewrite into
# the build output, so setting it only for `next start` leaves the old backend
# baked in. It must also be a real environment variable - npm's Windows shim
# drops inline VAR=value assignments.
$env:CGPU_API_URL = "http://10.20.24.32:8080"
$env:CGPU_PROMETHEUS_URL = "http://127.0.0.1:9090"
npx next build
npx next start -p 3100 -H 0.0.0.0
```

### 8. Keep the distro from idling out

WSL shuts the distro down when nothing holds it open, taking K3s, the agent and
both containers with it. Either leave a session open:

```powershell
wsl -d Ubuntu --exec sleep infinity          # in its own window
```

or install the systemd units (see the end of §1), which is the durable fix.

### Optional: put load on the GPU

Two matmul jobs sharing the card under separate quotas:

```bash
S=/var/run/cgpu/guard; mkdir -p $S
run() {   # job quota target seconds
  k3s ctr run --rm --runc-binary /usr/bin/nvidia-container-runtime \
    --env NVIDIA_VISIBLE_DEVICES=all --env NVIDIA_DRIVER_CAPABILITIES=compute,utility \
    --env LD_PRELOAD=/opt/cgpu/libcgpu_guard.so \
    --env CGPU_ALLOCATED_VRAM_MIB=$2 --env CGPU_JOB_ID=$1 \
    --env CGPU_GUARD_STATUS_FILE=/var/run/cgpu/guard/$1.json \
    --mount "type=bind,src=/opt/cgpu,dst=/opt/cgpu,options=rbind:ro" \
    --mount "type=bind,src=$S,dst=/var/run/cgpu/guard,options=rbind:rw" \
    docker.io/cgpu/workloads/gpu-matmul:1.0.0 "cgpu-$1-$RANDOM" \
    /usr/local/bin/cgpu_matmul --label $1 --size $3 --seconds $4
}
run demo-a 1536 4096 240 &
run demo-b 1024 3072 240 &

nvidia-smi --query-gpu=utilization.gpu,memory.used,temperature.gpu --format=csv,noheader
```

> `ctr run` replaces the image's ENTRYPOINT with whatever arguments follow the
> container name, so the binary path must be spelled out. Kubernetes honours the
> ENTRYPOINT, so a Job submitted through the portal needs no command.

### Stopping by hand

```bash
# monitoring (--rm, so killing the task removes the container)
for c in cgpu-prometheus cgpu-grafana; do
    k3s ctr task kill -s SIGTERM $c 2>/dev/null; k3s ctr container rm $c 2>/dev/null
done

# worker agent - match the exact path, since `pkill -f cgpu-worker-agent`
# also matches the command line running it and kills your own shell
kill $(pgrep -f '^/usr/local/bin/cgpu-worker-agent')
```

```powershell
# portal
Get-Process node | Where-Object { $_.Id -in (
    (netstat -ano | Select-String ':3100\s.*LISTENING') | ForEach-Object { ($_ -split '\s+')[-1] }
) } | Stop-Process -Force
```

---

## 2. Control plane (PC1, `10.20.24.31`)

```bash
systemctl start k3s                 # Kubernetes server
systemctl start cgpu-scheduler      # placement engine, :8082
kubectl get nodes -o wide           # every worker should be Ready
```

### Worker addresses — handled automatically

Nothing to do here any more. The scheduler's reconciler adopts each node's
`InternalIP` from Kubernetes every 30 s, so a laptop that rejoins on a new DHCP
lease is picked up on its own:

```text
[CGPU-Reconciler] node cgpu-gpu-worker-4050 address 10.20.24.35 -> 10.1.144.131
```

This used to be the single most confusing failure on the platform: the node
list was seeded with fixed addresses, the GPU workers are laptops on Wi-Fi, and
a stale address made the API probe port 9400 at the wrong host — so the portal
showed `AGENT_UNREACHABLE` and every measured figure read "unavailable" on
workers that were healthy and reporting the whole time.

[`scripts/gen-nodes-file.sh`](scripts/gen-nodes-file.sh) still exists and writes
the same information into `CGPU_NODES_FILE`, as a fallback if you ever need to
pin the inventory by hand.

### Cluster-wide monitoring

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
