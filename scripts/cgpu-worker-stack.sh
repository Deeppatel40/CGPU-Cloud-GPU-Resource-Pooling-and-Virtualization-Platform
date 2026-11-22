#!/usr/bin/env bash
# =============================================================================
#  cgpu-worker-stack.sh - bring up everything a CGPU GPU worker runs
# =============================================================================
#
#   sudo ./cgpu-worker-stack.sh start|stop|status|load
#
# Starts, in order: the K3s agent, the allocation guard, the worker agent and a
# node-local Prometheus + Grafana. Everything is idempotent, so re-running it is
# safe and only fills in what is missing.
#
# This lives in its own file rather than inline in start-cgpu.ps1 because
# PowerShell mangles double quotes when passing a command string to a native
# program: the bash below was silently truncated mid-`if`, which surfaced as
# "syntax error: unexpected end of file". Bash scripts belong in bash files.
#
# Prometheus here scrapes the worker agent over loopback, which deliberately
# sidesteps the control plane's hardcoded node IP - a worker keeps producing
# real telemetry even when the scheduler's recorded address for it is stale.
set -uo pipefail

REPO="${CGPU_REPO:-/mnt/d/Cloud_Project/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform}"
BASE=/var/lib/cgpu
MON="$BASE/monitoring"
PROM_IMAGE="${CGPU_PROM_IMAGE:-docker.io/prom/prometheus:v3.1.0}"
GRAF_IMAGE="${CGPU_GRAF_IMAGE:-docker.io/grafana/grafana:11.5.1}"

# Grafana is on 3001, not its default 3000, because 3000 is also Next.js's
# default - so any `npm start` in portal/ takes it first. Under WSL mirrored
# networking the distro shares the Windows ports, so a stray Node process on the
# Windows side makes Grafana fail with "address already in use" while nothing
# appears to be listening inside WSL. Moving Grafana ends that fight for good.
GRAF_PORT="${CGPU_GRAFANA_PORT:-3001}"

listening() { ss -lnt 2>/dev/null | grep -q ":$1[[:space:]]"; }
log() { printf '  %s\n' "$*"; }

# ---------------------------------------------------------------- start -------
start_k3s() {
    echo "== K3s agent"
    if [ "$(systemctl is-active k3s-agent 2>/dev/null)" != active ]; then
        systemctl start k3s-agent
        sleep 8
    fi
    log "k3s-agent: $(systemctl is-active k3s-agent 2>/dev/null)"
    log "GPU: $(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null || echo 'nvidia-smi unavailable')"
}

start_guard() {
    echo "== Allocation guard"
    # Every GPU pod bind-mounts /opt/cgpu. Without the library they fail to start
    # with "hostPath type check failed, /opt/cgpu is not a directory", which looks
    # like a Kubernetes problem rather than a missing build artefact.
    if [ -f /opt/cgpu/libcgpu_guard.so ]; then
        log "already installed ($(stat -c%s /opt/cgpu/libcgpu_guard.so) bytes)"
        return
    fi
    mkdir -p "$BASE/bin"
    if (cd "$REPO/pkg/guard" && ./build.sh "$BASE/bin" >/dev/null 2>&1); then
        install -D -m 644 "$BASE/bin/libcgpu_guard.so" /opt/cgpu/libcgpu_guard.so
        log "built and installed"
    else
        log "FAILED to build the guard - GPU jobs will not start"
    fi
}

start_agent() {
    echo "== Worker agent (:9400)"
    if listening 9400; then log "already listening"; return; fi
    if [ ! -x /usr/local/bin/cgpu-worker-agent ]; then
        log "binary missing; build it with:"
        log "  cd $REPO && go build -buildvcs=false -o /usr/local/bin/cgpu-worker-agent ./cmd/cgpu-worker-agent"
        return
    fi
    mkdir -p /var/run/cgpu/guard
    # setsid so it outlives the shell that started it.
    setsid /usr/local/bin/cgpu-worker-agent \
        -listen 0.0.0.0:9400 \
        -guard-status-dir /var/run/cgpu/guard \
        -reserve-mib 512 >/var/log/cgpu-agent.log 2>&1 < /dev/null &
    sleep 4
    listening 9400 && log "started" || log "failed to start - see /var/log/cgpu-agent.log"
}

start_prometheus() {
    echo "== Prometheus (:9090)"
    if listening 9090; then log "already listening"; return; fi
    if [ ! -f "$MON/prometheus/prometheus.yml" ]; then
        log "no config at $MON/prometheus/prometheus.yml - generate it from deploy/monitoring/"
        return
    fi
    mkdir -p "$MON/prom-data"
    # --rm cleans up on exit, but a hard kill leaves the container registered and
    # the next start fails on a duplicate id, so clear any corpse first.
    k3s ctr container rm cgpu-prometheus >/dev/null 2>&1
    setsid k3s ctr run --rm --net-host \
        --mount "type=bind,src=$MON/prometheus,dst=/etc/prometheus,options=rbind:ro" \
        --mount "type=bind,src=$MON/prom-data,dst=/prometheus,options=rbind:rw" \
        "$PROM_IMAGE" cgpu-prometheus \
        /bin/prometheus --config.file=/etc/prometheus/prometheus.yml \
            --storage.tsdb.path=/prometheus \
            --storage.tsdb.retention.time=7d \
            --web.listen-address=0.0.0.0:9090 >/var/log/cgpu-prometheus.log 2>&1 < /dev/null &
    for _ in $(seq 1 15); do listening 9090 && break; sleep 2; done
    listening 9090 && log "started" || log "failed to start - see /var/log/cgpu-prometheus.log"
}

start_grafana() {
    echo "== Grafana (:$GRAF_PORT)"
    if listening "$GRAF_PORT"; then log "already listening"; return; fi
    if [ ! -d "$MON/grafana/provisioning" ]; then
        log "no provisioning at $MON/grafana - generate it from deploy/monitoring/"
        return
    fi
    mkdir -p "$MON/grafana-data"
    chown -R 472:472 "$MON/grafana-data" 2>/dev/null   # grafana runs as uid 472
    k3s ctr container rm cgpu-grafana >/dev/null 2>&1
    setsid k3s ctr run --rm --net-host \
        --env GF_SECURITY_ADMIN_USER=admin \
        --env GF_SECURITY_ADMIN_PASSWORD=admin \
        --env GF_USERS_ALLOW_SIGN_UP=false \
        --env "GF_SERVER_HTTP_PORT=$GRAF_PORT" \
        --env GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=/var/lib/grafana/dashboards/cgpu-overview.json \
        --mount "type=bind,src=$MON/grafana-data,dst=/var/lib/grafana,options=rbind:rw" \
        --mount "type=bind,src=$MON/grafana/dashboards,dst=/var/lib/grafana/dashboards,options=rbind:ro" \
        --mount "type=bind,src=$MON/grafana/provisioning,dst=/etc/grafana/provisioning,options=rbind:ro" \
        "$GRAF_IMAGE" cgpu-grafana >/var/log/cgpu-grafana.log 2>&1 < /dev/null &
    for _ in $(seq 1 25); do listening "$GRAF_PORT" && break; sleep 2; done
    listening "$GRAF_PORT" && log "started" || log "failed to start - see /var/log/cgpu-grafana.log"
}

# ----------------------------------------------------------------- stop -------
do_stop() {
    echo "== Stopping"
    for c in cgpu-prometheus cgpu-grafana; do
        k3s ctr task kill -s SIGTERM "$c" >/dev/null 2>&1
        k3s ctr container rm "$c" >/dev/null 2>&1
    done
    log "prometheus + grafana stopped"
    # Match the exact path: `pkill -f cgpu-worker-agent` also matches this very
    # script's own command line and kills the shell running it.
    p=$(pgrep -f '^/usr/local/bin/cgpu-worker-agent' | head -1)
    if [ -n "$p" ]; then kill "$p" && log "worker agent stopped (pid $p)"; else log "worker agent was not running"; fi
    log "k3s-agent left running so the node stays in the cluster"
}

# --------------------------------------------------------------- status -------
do_status() {
    echo "== Status"
    printf '  %-16s %s\n' "k3s-agent" "$(systemctl is-active k3s-agent 2>/dev/null)"
    for pair in "worker agent:9400:/healthz" "Prometheus:9090:/-/ready" "Grafana:$GRAF_PORT:/api/health"; do
        name=${pair%%:*}; rest=${pair#*:}; port=${rest%%:*}; path=${rest#*:}
        code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$port$path" 2>/dev/null)
        if [ "${code:-000}" -ge 200 ] 2>/dev/null && [ "${code:-000}" -lt 400 ] 2>/dev/null; then
            printf '  %-16s :%-5s up   (HTTP %s)\n' "$name" "$port" "$code"
        else
            printf '  %-16s :%-5s down\n' "$name" "$port"
        fi
    done
    printf '  %-16s %s\n' "guard" "$([ -f /opt/cgpu/libcgpu_guard.so ] && echo installed || echo MISSING)"
    echo "  GPU: $(nvidia-smi --query-gpu=utilization.gpu,memory.used,temperature.gpu --format=csv,noheader 2>/dev/null)"
}

# ----------------------------------------------------------------- load -------
do_load() {
    echo "== GPU load"
    if [ ! -x "$BASE/bin/loadgen.sh" ]; then log "no $BASE/bin/loadgen.sh"; return; fi
    setsid bash "$BASE/bin/loadgen.sh" "${2:-240}" >/var/log/cgpu-load.log 2>&1 < /dev/null &
    sleep 25
    echo "  GPU: $(nvidia-smi --query-gpu=utilization.gpu,memory.used,temperature.gpu --format=csv,noheader 2>/dev/null)"
}

case "${1:-start}" in
    start)
        start_k3s; start_guard; start_agent; start_prometheus; start_grafana
        echo; do_status
        ;;
    stop)   do_stop ;;
    status) do_status ;;
    load)   do_load "$@" ;;
    *) echo "usage: $0 start|stop|status|load" >&2; exit 2 ;;
esac
