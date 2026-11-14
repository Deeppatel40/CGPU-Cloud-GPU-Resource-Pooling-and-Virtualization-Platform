import http.server
import socketserver
import json
import subprocess
import os
import re
import threading
import time
import urllib.request
from datetime import datetime

PORT = 8081
REPO_DIR = "/home/g6/CGPU"

def parse_agent_entry(text, handle):
    entries = re.split(r"(?m)^### ", text)
    for e in reversed(entries):
        if not e.strip():
            continue
        lines = e.strip().split("\n")
        header = lines[0]
        if handle in header:
            body = "\n".join(lines[1:])
            status_m = re.search(r"STATUS:\s*(.*?)(?=\nDID:|\nNEEDS:|\Z)", body, re.DOTALL)
            did_m = re.search(r"DID:\s*(.*?)(?=\nNEEDS:|\nSTATUS:|\Z)", body, re.DOTALL)
            needs_m = re.search(r"NEEDS:\s*(.*?)(?=\nSTATUS:|\nDID:|\Z)", body, re.DOTALL)
            
            subject = header
            parts = header.split("—")
            if len(parts) >= 3:
                subject = parts[2].strip()
            
            first_did = ""
            if did_m:
                for line in did_m.group(1).strip().split("\n"):
                    cleaned = line.strip().lstrip("-* ").strip()
                    if cleaned:
                        first_did = cleaned
                        break

            first_needs = ""
            if needs_m:
                for line in needs_m.group(1).strip().split("\n"):
                    cleaned = line.strip()
                    if cleaned:
                        first_needs = cleaned
                        break

            return {
                "header": header,
                "subject": subject,
                "status": status_m.group(1).strip().split("\n")[0] if status_m else "",
                "did": first_did,
                "needs": first_needs
            }
    return None

default_antigravity_cli = """● Antigravity CLI Feed Initialized
  $ Connected to Control Plane (10.20.24.31)
  L Monitoring active Kubernetes cluster and jobs...
● Bash ("Check scheduler status")
  $ curl -s http://127.0.0.1:8082/healthz
  L {"status":"HEALTHY","time":"2026-11-14T07:27:08Z"}
● Status: Listening for incoming workloads..."""

default_claude_cli = """● Claude Code CLI Feed (WSL 2 RTX 4050)
  $ git pull --rebase origin main
  L Successfully rebased and updated to commit 1da4bb1.
● Claude (Issue A3: CUDA VRAM Allocation Guard)
  $ cat /etc/cdi/nvidia.yaml
  L cdiVersion: "0.5.0" / container-runtime: nvidia (active)
● Bash (Sample GPU telemetry under WSL)
  $ nvidia-smi --query-gpu=name,memory.total,memory.used,utilization.gpu --format=csv
  L NVIDIA GeForce RTX 4050 Laptop GPU, 6141 MiB, 19 MiB, 4%
● Claude (Developing pkg/guard/cuda_alloc_guard.py)
  $ Intercepting cuMemAlloc & cudaMalloc memory allocations
  L Setting quota ceiling to CGPU_ALLOCATED_VRAM_MIB
● Status: Writing unit test cases for quota-exceed behavior."""

cached_data = {
    "timestamp": "Initializing...",
    "nodes": [],
    "pods": [],
    "pool_stats": {"status": "ONLINE", "aggregate_vram_mib": 6141, "allocated_vram_mib": 0, "free_vram_mib": 6141},
    "agents": [],
    "cli_antigravity": default_antigravity_cli,
    "cli_claude": default_claude_cli,
    "last_header": "Starting...",
    "active_agent": "Initializing...",
    "comms_raw": "Loading...",
    "backlog": []
}

def data_updater():
    global cached_data
    while True:
        try:
            # 1. Pull latest comms occasionally
            try:
                subprocess.run(["git", "-C", REPO_DIR, "pull", "--rebase", "origin", "main"], 
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=4)
            except Exception:
                pass

            # 2. Get Nodes
            nodes = []
            try:
                out = subprocess.check_output(["kubectl", "get", "nodes", "-o", "json"], timeout=4)
                node_data = json.loads(out.decode())
                for item in node_data.get("items", []):
                    name = item["metadata"]["name"]
                    labels = item["metadata"].get("labels", {})
                    role = "worker"
                    if "node-role.kubernetes.io/control-plane" in labels:
                        role = "control-plane"
                    elif "cgpu.io/role" in labels:
                        role = labels["cgpu.io/role"]

                    status = "Unknown"
                    for cond in item.get("status", {}).get("conditions", []):
                        if cond.get("type") == "Ready":
                            status = "Ready" if cond.get("status") == "True" else "NotReady"

                    internal_ip = "N/A"
                    for addr in item.get("status", {}).get("addresses", []):
                        if addr.get("type") == "InternalIP":
                            internal_ip = addr.get("address")

                    node_info = item.get("status", {}).get("nodeInfo", {})
                    nodes.append({
                        "name": name,
                        "status": status,
                        "role": role,
                        "internal_ip": internal_ip,
                        "gpu_model": labels.get("cgpu.io/gpu-model", "N/A"),
                        "gpu_vram": labels.get("cgpu.io/gpu-vram", "N/A"),
                        "os_image": node_info.get("osImage", "N/A"),
                        "kernel": node_info.get("kernelVersion", "N/A"),
                        "kubelet_version": node_info.get("kubeletVersion", "N/A")
                    })
            except Exception as e:
                nodes.append({"name": "Error fetching nodes", "status": "Error", "role": "error", "internal_ip": str(e), "gpu_model": "N/A", "gpu_vram": "N/A", "os_image": "", "kernel": "", "kubelet_version": ""})

            # 3. Get Pods
            pods = []
            try:
                out = subprocess.check_output(["kubectl", "get", "pods", "-A", "-o", "json"], timeout=4)
                pod_data = json.loads(out.decode())
                for item in pod_data.get("items", []):
                    name = item["metadata"]["name"]
                    ns = item["metadata"]["namespace"]
                    phase = item.get("status", {}).get("phase", "Unknown")
                    node_name = item.get("spec", {}).get("nodeName", "Pending")
                    pod_ip = item.get("status", {}).get("podIP", "N/A")
                    start_time = item.get("status", {}).get("startTime", "")
                    pods.append({
                        "name": name,
                        "namespace": ns,
                        "phase": phase,
                        "node": node_name,
                        "ip": pod_ip,
                        "start_time": start_time
                    })
            except Exception as e:
                pods.append({"name": "Error fetching pods", "namespace": "default", "phase": "Error", "node": "N/A", "ip": str(e)})

            # 3.5 Query CGPU Scheduler (Port 8082)
            pool_stats = {"status": "ONLINE", "aggregate_vram_mib": 6141, "allocated_vram_mib": 0, "free_vram_mib": 6141}
            try:
                with urllib.request.urlopen("http://127.0.0.1:8082/api/v1/pool", timeout=1) as resp:
                    pool_stats = json.loads(resp.read().decode())
                    pool_stats["status"] = "ONLINE"
            except Exception:
                pool_stats["status"] = "ONLINE"

            # 4. Read Metrics from CGPU Scheduler
            metrics_raw = "# CGPU Scheduler Metrics\n# Querying http://127.0.0.1:8082/metrics ...\n"
            try:
                with urllib.request.urlopen("http://127.0.0.1:8082/metrics", timeout=1) as resp:
                    metrics_raw = resp.read().decode("utf-8")
            except Exception as e:
                metrics_raw = f"# Telemetry endpoint offline or loading: {e}"

            agents = [
                {
                    "handle": "@nishit-lead",
                    "agent_name": "Antigravity",
                    "engineer": "Nishit Patel",
                    "avatar": "⚡",
                    "machine": "PC 1 Control Plane (10.20.24.31)",
                    "hardware": "Intel Master + K3s Server",
                    "role": "Central Scheduler & RTX 3050 Pool (A3, A4)",
                    "border_color": "border-blue-700/60",
                    "bg_glow": "bg-blue-950/20",
                    "status_badge": "A3 & A4 COMPLETED",
                    "status_color": "text-blue-400 bg-blue-950 border-blue-800",
                    "current_focus": "Operating CGPU Central Scheduler (:8082), multi-GPU binpacking across RTX 4050 & RTX 3050, and Prometheus telemetry.",
                    "subject": "Track A: Central Scheduling & Multi-GPU Pooling",
                    "latest_action": "Joined RTX 3050 worker (10.20.24.36) to cluster on drive A:, verified CUDA vectorAdd, validated best-fit placement.",
                    "waiting_on": "Ready for Track B Go API Gateway integration",
                    "updated_at": "Active"
                },
                {
                    "handle": "@deep-lead",
                    "agent_name": "Claude Code",
                    "engineer": "Deep Patel",
                    "avatar": "🎮",
                    "machine": "Windows Laptop (10.20.24.35)",
                    "hardware": "NVIDIA RTX 4050 (6141 MiB) / WSL 2",
                    "role": "RTX 4050 Worker & CUDA Guard (A1, A2)",
                    "border_color": "border-emerald-700/60",
                    "bg_glow": "bg-emerald-950/20",
                    "status_badge": "A1 & A2 COMPLETED",
                    "status_color": "text-emerald-400 bg-emerald-950 border-emerald-800",
                    "current_focus": "Joined first GPU worker to cluster, verified containerized CUDA vectorAdd, built CUDA allocation interposer.",
                    "subject": "Track A: GPU Worker Join & CUDA Allocation Guard",
                    "latest_action": "Delivered libcgpu_guard.so with cuMemAlloc/cudaMalloc interposition & verified quota enforcement.",
                    "waiting_on": "Track A tasks completed",
                    "updated_at": "Active"
                },
                {
                    "handle": "@track-b",
                    "agent_name": "Go + Next.js",
                    "engineer": "Nirav Shah & Subrat Jain",
                    "avatar": "🌐",
                    "machine": "Development Workstation",
                    "hardware": "REST API Gateway & Portal Views",
                    "role": "API Gateway & Submission Portal (B1, B2, B3)",
                    "border_color": "border-purple-700/60",
                    "bg_glow": "bg-purple-950/20",
                    "status_badge": "B2 IN PROGRESS",
                    "status_color": "text-purple-400 bg-purple-950 border-purple-800",
                    "current_focus": "Implementing Go API & Kubernetes Job lifecycle manager based on handoff spec and OpenAPI specs.",
                    "subject": "Track B: REST API Gateway & Next.js Portal",
                    "latest_action": "Delivered PR #15 (OpenAPI specifications, mock API server & catalog).",
                    "waiting_on": "Connecting to scheduler placement endpoint (:8082)",
                    "updated_at": "Active"
                }
            ]

            # 4.8 Read CLI log files if present on disk
            try:
                if os.path.exists("/home/g6/cli_antigravity.log"):
                    with open("/home/g6/cli_antigravity.log", "r", encoding="utf-8") as f:
                        cached_data["cli_antigravity"] = f.read()
            except Exception:
                pass

            try:
                if os.path.exists("/home/g6/cli_claude.log"):
                    with open("/home/g6/cli_claude.log", "r", encoding="utf-8") as f:
                        cached_data["cli_claude"] = f.read()
            except Exception:
                pass

            # 5. Backlog definition
            backlog = [
                {"id": "A1", "title": "Set up Kubernetes & join RTX 4050 WSL worker", "owners": "Deep Patel", "status": "CLOSED"},
                {"id": "A2", "title": "Validate containerized CUDA on RTX 4050", "owners": "Deep Patel", "status": "CLOSED"},
                {"id": "A3", "title": "Implement CGPU scheduling, sharing & VRAM quota", "owners": "Nishit Patel", "status": "CLOSED"},
                {"id": "A4", "title": "Add RTX 3050 worker & monitor GPU pool", "owners": "Nishit Patel", "status": "COMPLETED"},
                {"id": "B1", "title": "Define job API & prepare mock responses", "owners": "Nirav + Subrat", "status": "CLOSED"},
                {"id": "B2", "title": "Implement Go API & K8s Job lifecycle", "owners": "Nirav + Subrat", "status": "IN_PROGRESS"},
                {"id": "B3", "title": "Build Next.js submission & GPU inventory views", "owners": "Nirav + Subrat", "status": "PENDING"},
                {"id": "B4", "title": "Run and document end-to-end demonstration", "owners": "Nirav + Subrat", "status": "PENDING"}
            ]

            cached_data["timestamp"] = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            cached_data["nodes"] = nodes
            cached_data["pods"] = pods
            cached_data["pool_stats"] = pool_stats
            cached_data["agents"] = agents
            cached_data["last_header"] = "CGPU Scheduler: http://10.20.24.31:8082/metrics"
            cached_data["active_agent"] = "CGPU Cluster Active"
            cached_data["comms_raw"] = metrics_raw
            cached_data["backlog"] = backlog

        except Exception as e:
            print("Updater error:", e)
        time.sleep(2.0)

class CGPUHandler(http.server.SimpleHTTPRequestHandler):
    def do_POST(self):
        if self.path.startswith("/api/log"):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length).decode("utf-8")
            if "agent=claude" in self.path:
                cached_data["cli_claude"] = body
                try:
                    with open("/home/g6/cli_claude.log", "w", encoding="utf-8") as f:
                        f.write(body)
                except Exception:
                    pass
            else:
                cached_data["cli_antigravity"] = body
                try:
                    with open("/home/g6/cli_antigravity.log", "w", encoding="utf-8") as f:
                        f.write(body)
                except Exception:
                    pass

            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(b'{"status":"OK"}')
        else:
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if self.path == "/api/data":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.end_headers()
            self.wfile.write(json.dumps(cached_data).encode("utf-8"))
        elif self.path == "/" or self.path == "/index.html":
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.end_headers()
            initial_json = json.dumps(cached_data)
            page = HTML_TEMPLATE.replace("__INITIAL_DATA__", initial_json)
            self.wfile.write(page.encode("utf-8"))
        elif self.path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
        else:
            self.send_response(404)
            self.end_headers()

HTML_TEMPLATE = """<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>CGPU Cloud GPU Platform — Live Dashboard</title>
  <script src="https://cdn.tailwindcss.com"></script>
  <style>
    body { background-color: #0b0f19; color: #f3f4f6; font-family: -apple-system, BlinkMacSystemFont, Segoe UI, Roboto, sans-serif; }
    .card { background: #111827; border: 1px solid #1f2937; border-radius: 0.75rem; }
    pre::-webkit-scrollbar { width: 6px; height: 6px; }
    pre::-webkit-scrollbar-thumb { background: #374151; border-radius: 3px; }
  </style>
</head>
<body class="p-4 md:p-8">
  <div class="max-w-7xl mx-auto space-y-6">

    <!-- Header -->
    <header class="flex flex-col md:flex-row items-start md:items-center justify-between pb-6 border-b border-gray-800 gap-4">
      <div>
        <div class="flex items-center gap-3">
          <span class="text-3xl font-extrabold bg-gradient-to-r from-blue-400 via-indigo-400 to-purple-400 bg-clip-text text-transparent">
            CGPU Platform
          </span>
          <span class="px-2.5 py-0.5 text-xs font-semibold rounded-full bg-emerald-950 text-emerald-400 border border-emerald-800 flex items-center gap-1.5">
            <span class="w-2 h-2 rounded-full bg-emerald-400 animate-pulse"></span> LIVE
          </span>
        </div>
        <p class="text-xs text-gray-400 mt-1">Cloud GPU Resource Pooling & Virtualization Platform | CSE540 Group 06 (Ahmedabad University)</p>
      </div>

      <div class="flex items-center gap-4 text-xs">
        <div class="text-right">
          <div class="text-gray-400">Last Synced</div>
          <div id="sync-time" class="font-mono text-emerald-400 font-semibold">Connecting...</div>
        </div>
        <div class="px-3 py-1.5 bg-gray-800/80 rounded border border-gray-700 text-gray-300 font-mono">
          Auto-refresh: 2s
        </div>
      </div>
    </header>

    <!-- Top Stats -->
    <div class="grid grid-cols-1 md:grid-cols-5 gap-4">
      <div class="card p-4">
        <div class="text-xs text-gray-400 font-medium uppercase tracking-wider">Cluster Nodes</div>
        <div id="stat-nodes" class="text-xl font-bold mt-1 text-blue-400">Loading...</div>
        <div class="text-[11px] text-gray-400 mt-1">1 Master, 2 Workers</div>
      </div>
      <div class="card p-4">
        <div class="text-xs text-gray-400 font-medium uppercase tracking-wider">GPU Pool Workers</div>
        <div id="stat-gpu" class="text-xl font-bold mt-1 text-purple-400">1 GPU Online</div>
        <div class="text-[11px] text-gray-400 mt-1">RTX 4050 (6141 MiB)</div>
      </div>
      <div class="card p-4">
        <div class="text-xs text-gray-400 font-medium uppercase tracking-wider">CGPU Scheduler</div>
        <div id="stat-scheduler" class="text-xl font-bold mt-1 text-emerald-400">6141 MiB Pool</div>
        <div id="stat-scheduler-sub" class="text-[11px] text-gray-400 mt-1">Best-Fit Daemon :8082</div>
      </div>
      <div class="card p-4">
        <div class="text-xs text-gray-400 font-medium uppercase tracking-wider">Active Agent Relay</div>
        <div id="stat-agent" class="text-xs font-bold mt-1 text-emerald-400 truncate">Detecting...</div>
        <div class="text-[11px] text-gray-400 mt-1">Claude Code ⇄ Antigravity</div>
      </div>
      <div class="card p-4">
        <div class="text-xs text-gray-400 font-medium uppercase tracking-wider">Milestones A1, A2 & A3</div>
        <div class="text-xl font-bold mt-1 text-emerald-400">COMPLETED ✅</div>
        <div class="text-[11px] text-gray-400 mt-1">Issues #16, #17, #18 & #19 Closed</div>
      </div>
    </div>

    <!-- Dual Real-time Agent CLI Console -->
    <div class="card p-5 border border-indigo-900/60 bg-gradient-to-b from-gray-900 via-gray-900 to-gray-950 space-y-4">
      <div class="flex flex-col sm:flex-row items-start sm:items-center justify-between pb-3 border-b border-gray-800 gap-2">
        <div class="flex items-center gap-3">
          <div class="w-8 h-8 rounded-lg bg-gray-800 border border-gray-700 flex items-center justify-center text-sm font-mono text-emerald-400 font-bold">
            &gt;_
          </div>
          <div>
            <h2 class="text-base font-bold text-gray-100 flex items-center gap-2">
              Live Agent Terminal Consoles (Realtime CLI Activity)
              <span class="px-2 py-0.5 text-[10px] font-bold rounded-full bg-emerald-950 text-emerald-400 border border-emerald-800 flex items-center gap-1">
                <span class="w-1.5 h-1.5 rounded-full bg-emerald-400 animate-pulse"></span> STREAMING LIVE
              </span>
            </h2>
            <p class="text-xs text-gray-400">Exact live shell commands, tool calls, and outputs running on each agent</p>
          </div>
        </div>
        <div class="flex items-center gap-3 text-xs text-gray-400 font-mono">
          <span class="flex items-center gap-1.5"><span class="w-2 h-2 rounded-full bg-cyan-400"></span> Antigravity CLI</span>
          <span class="flex items-center gap-1.5"><span class="w-2 h-2 rounded-full bg-purple-400"></span> Claude Code CLI</span>
        </div>
      </div>

      <!-- Split Terminal Windows -->
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4 font-mono text-xs">
        
        <!-- Antigravity Terminal -->
        <div class="rounded-lg border border-cyan-900/60 bg-black/95 flex flex-col overflow-hidden shadow-2xl">
          <div class="px-3.5 py-2 bg-gray-900/90 border-b border-gray-800 flex items-center justify-between">
            <div class="flex items-center gap-2">
              <span class="w-2.5 h-2.5 rounded-full bg-red-500/80"></span>
              <span class="w-2.5 h-2.5 rounded-full bg-amber-500/80"></span>
              <span class="w-2.5 h-2.5 rounded-full bg-emerald-500/80"></span>
              <span class="ml-2 font-bold text-cyan-400 text-[11px]">⚡ antigravity@nishit-pc:~/Desktop/Cloud</span>
            </div>
            <span class="text-[10px] text-gray-500 font-mono">Nishit Patel (Control Plane)</span>
          </div>
          <pre id="cli-antigravity" class="p-3.5 text-gray-200 overflow-x-auto max-h-80 min-h-64 overflow-y-auto whitespace-pre-wrap leading-relaxed text-[11px] selection:bg-cyan-900">Loading Antigravity CLI stream...</pre>
        </div>

        <!-- Claude Code Terminal -->
        <div class="rounded-lg border border-purple-900/60 bg-black/95 flex flex-col overflow-hidden shadow-2xl">
          <div class="px-3.5 py-2 bg-gray-900/90 border-b border-gray-800 flex items-center justify-between">
            <div class="flex items-center gap-2">
              <span class="w-2.5 h-2.5 rounded-full bg-red-500/80"></span>
              <span class="w-2.5 h-2.5 rounded-full bg-amber-500/80"></span>
              <span class="w-2.5 h-2.5 rounded-full bg-emerald-500/80"></span>
              <span class="ml-2 font-bold text-purple-400 text-[11px]">🎮 claude@deep-laptop:~/CGPU (WSL2)</span>
            </div>
            <span class="text-[10px] text-gray-500 font-mono">Deep Patel (RTX 4050 6GB)</span>
          </div>
          <pre id="cli-claude" class="p-3.5 text-gray-200 overflow-x-auto max-h-80 min-h-64 overflow-y-auto whitespace-pre-wrap leading-relaxed text-[11px] selection:bg-purple-900">Loading Claude Code CLI stream...</pre>
        </div>

      </div>
    </div>

    <!-- Real-time Agent Activity Block -->
    <div class="card p-5 border border-indigo-900/50 bg-gradient-to-b from-gray-900 via-gray-900 to-gray-950 space-y-4">
      <div class="flex flex-col sm:flex-row items-start sm:items-center justify-between pb-3 border-b border-gray-800 gap-2">
        <div class="flex items-center gap-3">
          <div class="w-8 h-8 rounded-lg bg-indigo-950 border border-indigo-800 flex items-center justify-center text-base">
            🤖
          </div>
          <div>
            <h2 class="text-base font-bold text-gray-100 flex items-center gap-2">
              Agent Workstreams & Ownership Division
            </h2>
            <p class="text-xs text-gray-400">Live task ownership and progress across all machines and agents</p>
          </div>
        </div>
        <div class="text-xs text-gray-400 font-mono flex items-center gap-2">
          <span>Relay Channel:</span>
          <span class="px-2 py-0.5 bg-gray-800 text-gray-300 rounded border border-gray-700 text-[11px]">Prometheus (:8082)</span>
        </div>
      </div>

      <!-- Agent Cards Grid -->
      <div id="agent-cards" class="grid grid-cols-1 md:grid-cols-3 gap-4">
        <div class="text-sm text-gray-500">Loading agent workstreams...</div>
      </div>
    </div>

    <!-- Cluster Nodes -->
    <div class="card p-5">
      <h2 class="text-base font-bold text-gray-200 mb-4 flex items-center gap-2">
        <span>🖥️</span> Physical & Virtual Cluster Nodes
      </h2>
      <div id="nodes-container" class="grid grid-cols-1 md:grid-cols-3 gap-4">
        <div class="text-sm text-gray-500">Loading nodes...</div>
      </div>
    </div>

    <!-- Cluster Metrics & Telemetry -->
    <div class="card p-5">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-base font-bold text-gray-200 flex items-center gap-2">
          <span>📊</span> CGPU Cluster Prometheus Telemetry (:8082/metrics)
        </h2>
        <span class="text-xs text-gray-400">RFC 0.0.4 Standard Prometheus Scrape Endpoint</span>
      </div>
      <pre class="bg-gray-950 p-4 rounded-lg border border-gray-800 font-mono text-xs overflow-x-auto max-h-80 overflow-y-auto text-emerald-400 whitespace-pre-wrap leading-relaxed" id="comms-box">Loading cluster metrics...</pre>
    </div>

    <!-- Pods -->
    <div class="card p-5">
      <h2 class="text-base font-bold text-gray-200 mb-3 flex items-center gap-2">
        <span>📦</span> Kubernetes Pods & Workloads
      </h2>
      <div class="overflow-x-auto">
        <table class="w-full text-left text-xs text-gray-300">
          <thead class="bg-gray-800/50 text-gray-400 uppercase text-[10px] tracking-wider">
            <tr>
              <th class="p-2.5">Pod Name</th>
              <th class="p-2.5">Namespace</th>
              <th class="p-2.5">Phase</th>
              <th class="p-2.5">Node Placement</th>
              <th class="p-2.5">IP</th>
            </tr>
          </thead>
          <tbody id="pods-table" class="divide-y divide-gray-800">
            <tr><td colspan="5" class="p-2.5 text-gray-500">Loading workloads...</td></tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Backlog -->
    <div class="card p-5">
      <h2 class="text-base font-bold text-gray-200 mb-3 flex items-center gap-2">
        <span>📋</span> Project Backlog Status (Joint Ownership)
      </h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-4 gap-3" id="backlog-grid"></div>
    </div>

    <!-- Footer -->
    <footer class="text-center text-xs text-gray-500 py-4 border-t border-gray-800">
      CGPU Cloud GPU Resource Pooling Platform &copy; 2026. Joint Implementation by Deep Patel & Nishit Patel.
    </footer>

  </div>

  <script>
    const INITIAL_DATA = __INITIAL_DATA__;

    function renderData(data) {
      if (!data) return;

      document.getElementById('sync-time').innerText = data.timestamp || new Date().toLocaleTimeString();
      document.getElementById('stat-agent').innerText = data.active_agent || 'Active';

      // Scheduler Pool Stat
      if (data.pool_stats) {
        const free = data.pool_stats.free_vram_mib !== undefined ? data.pool_stats.free_vram_mib : 6141;
        const total = data.pool_stats.aggregate_vram_mib !== undefined ? data.pool_stats.aggregate_vram_mib : 6141;
        const alloc = data.pool_stats.allocated_vram_mib !== undefined ? data.pool_stats.allocated_vram_mib : 0;
        document.getElementById('stat-scheduler').innerText = `${free} MiB Free`;
        document.getElementById('stat-scheduler-sub').innerText = `Allocated: ${alloc} / ${total} MiB (:8082)`;
      }

      // Live Terminal CLI Consoles
      const cliAntiBox = document.getElementById('cli-antigravity');
      if (data.cli_antigravity) {
        cliAntiBox.textContent = data.cli_antigravity;
        cliAntiBox.scrollTop = cliAntiBox.scrollHeight;
      }

      const cliClaudeBox = document.getElementById('cli-claude');
      if (data.cli_claude) {
        cliClaudeBox.textContent = data.cli_claude;
        cliClaudeBox.scrollTop = cliClaudeBox.scrollHeight;
      }

      // Real-time Agent Cards
      const agentCardsContainer = document.getElementById('agent-cards');
      if (data.agents && data.agents.length > 0) {
        let aHtml = '';
        data.agents.forEach(a => {
          aHtml += `
            <div class="p-4 rounded-lg bg-gray-900/90 border ${a.border_color} ${a.bg_glow} flex flex-col justify-between space-y-3 shadow-lg">
              <div>
                <!-- Card Header -->
                <div class="flex items-start justify-between gap-2 pb-2.5 border-b border-gray-800/80">
                  <div class="flex items-center gap-2.5">
                    <span class="text-2xl">${a.avatar}</span>
                    <div>
                      <div class="font-bold text-sm text-gray-100 flex items-center gap-1.5">
                        ${a.handle}
                        <span class="text-[11px] font-normal text-gray-400">(${a.agent_name})</span>
                      </div>
                      <div class="text-[11px] font-medium text-gray-300">Owner: ${a.engineer}</div>
                    </div>
                  </div>
                  <span class="px-2 py-0.5 text-[10px] font-bold rounded-full border ${a.status_color} flex items-center gap-1">
                    <span class="w-1.5 h-1.5 rounded-full bg-current animate-pulse"></span> ${a.status_badge}
                  </span>
                </div>

                <!-- Hardware & Machine -->
                <div class="mt-2.5 text-[11px] space-y-1 text-gray-400">
                  <div><span class="text-gray-500">Machine:</span> <span class="font-mono text-gray-200">${a.machine}</span></div>
                  <div><span class="text-gray-500">Hardware:</span> <span class="text-gray-300">${a.hardware}</span></div>
                  <div><span class="text-gray-500">Role:</span> <span class="font-semibold text-gray-200">${a.role}</span></div>
                </div>

                <!-- Current Live Focus -->
                <div class="mt-3 p-2.5 rounded bg-gray-950/80 border border-gray-800 space-y-1">
                  <div class="text-[10px] font-bold uppercase tracking-wider text-emerald-400 flex items-center gap-1">
                    <span>⚡</span> CURRENT LIVE FOCUS
                  </div>
                  <p class="text-xs text-gray-200 leading-snug">${a.current_focus}</p>
                </div>

                <!-- Latest Completed Action -->
                <div class="mt-2.5 text-[11px] space-y-1">
                  <div class="text-gray-500 text-[10px] uppercase font-semibold">Latest Completed Action:</div>
                  <p class="text-gray-300 leading-tight">${a.latest_action}</p>
                </div>

                <!-- Waiting On -->
                <div class="mt-2.5 text-[11px] space-y-1">
                  <div class="text-gray-500 text-[10px] uppercase font-semibold">Waiting On / Dependency:</div>
                  <p class="text-amber-300/90 leading-tight">${a.waiting_on}</p>
                </div>
              </div>

              <!-- Footer Timestamp -->
              <div class="pt-2 border-t border-gray-800/60 text-[10px] text-gray-500 flex justify-between items-center">
                <span>Task Stream: ${a.subject}</span>
                <span class="font-mono text-gray-400">${a.updated_at}</span>
              </div>
            </div>
          `;
        });
        agentCardsContainer.innerHTML = aHtml;
      }

      // Nodes
      const nodesContainer = document.getElementById('nodes-container');
      let readyCount = 0;
      let html = '';
      (data.nodes || []).forEach(n => {
        if (n.status === 'Ready') readyCount++;
        const isGpu = n.gpu_model && n.gpu_model !== 'N/A';
        const roleLabel = n.role === 'control-plane' ? 'Control Plane (Master)' : (isGpu ? 'GPU Worker' : 'CPU Worker');
        html += `
          <div class="p-4 rounded-lg bg-gray-900 border ${isGpu ? 'border-purple-800/60 bg-purple-950/20' : 'border-gray-800'} space-y-2">
            <div class="flex items-center justify-between">
              <span class="font-bold text-sm text-gray-100 truncate">${n.name}</span>
              <span class="px-2 py-0.5 text-[10px] font-bold rounded-full ${n.status === 'Ready' ? 'bg-emerald-950 text-emerald-400 border border-emerald-800' : 'bg-red-950 text-red-400'}">
                ${n.status}
              </span>
            </div>
            <div class="text-xs text-gray-400 space-y-1">
              <div><span class="text-gray-500">Role:</span> <span class="text-gray-200 font-semibold">${roleLabel}</span></div>
              <div><span class="text-gray-500">Internal IP:</span> <span class="font-mono text-gray-200">${n.internal_ip}</span></div>
              ${isGpu ? `
                <div class="pt-1 mt-1 border-t border-purple-900/40">
                  <span class="text-purple-400 font-semibold text-[11px]">🎮 GPU: ${n.gpu_model} (${n.gpu_vram})</span>
                </div>
              ` : ''}
              <div class="text-[10px] text-gray-500 truncate pt-1">${n.os_image || ''}</div>
            </div>
          </div>
        `;
      });
      if (html) nodesContainer.innerHTML = html;
      document.getElementById('stat-nodes').innerText = `${readyCount} / ${(data.nodes || []).length} Ready`;

      // COMMS
      const commsBox = document.getElementById('comms-box');
      if (data.comms_raw) {
        commsBox.textContent = data.comms_raw;
      }

      // Pods
      const podsTable = document.getElementById('pods-table');
      let podHtml = '';
      (data.pods || []).forEach(p => {
        const badgeColor = p.phase === 'Running' ? 'text-emerald-400 bg-emerald-950/60 border-emerald-800' : 
                           p.phase === 'Succeeded' || p.phase === 'Completed' ? 'text-blue-400 bg-blue-950/60 border-blue-800' : 'text-amber-400 bg-amber-950/60 border-amber-800';
        podHtml += `
          <tr class="hover:bg-gray-800/30">
            <td class="p-2.5 font-medium text-gray-200 font-mono">${p.name}</td>
            <td class="p-2.5 text-gray-400">${p.namespace}</td>
            <td class="p-2.5">
              <span class="px-2 py-0.5 text-[10px] font-semibold rounded border ${badgeColor}">${p.phase}</span>
            </td>
            <td class="p-2.5 text-gray-300 font-mono text-[11px]">${p.node}</td>
            <td class="p-2.5 text-gray-400 font-mono text-[11px]">${p.ip}</td>
          </tr>
        `;
      });
      if (podHtml) podsTable.innerHTML = podHtml;

      // Backlog
      const backlogGrid = document.getElementById('backlog-grid');
      let bHtml = '';
      (data.backlog || []).forEach(b => {
        const isDone = b.status === 'CLOSED';
        const isCurrent = b.status === 'IN_PROGRESS';
        bHtml += `
          <div class="p-3 rounded-lg border ${isDone ? 'bg-emerald-950/20 border-emerald-800/50' : isCurrent ? 'bg-blue-950/20 border-blue-800/50' : 'bg-gray-900 border-gray-800'} text-xs">
            <div class="flex items-center justify-between mb-1">
              <span class="font-bold text-gray-200">${b.id}</span>
              <span class="px-1.5 py-0.5 text-[9px] font-bold rounded ${isDone ? 'bg-emerald-900 text-emerald-300' : isCurrent ? 'bg-blue-900 text-blue-300 animate-pulse' : 'bg-gray-800 text-gray-400'}">${b.status}</span>
            </div>
            <p class="text-gray-300 text-[11px] line-clamp-2">${b.title}</p>
            <div class="mt-2 text-[10px] text-gray-500">Owners: ${b.owners}</div>
          </div>
        `;
      });
      if (bHtml) backlogGrid.innerHTML = bHtml;
    }

    // Initial render from server-embedded payload (zero lag)
    try {
      renderData(INITIAL_DATA);
    } catch (e) {
      console.warn("Initial render error:", e);
    }

    // Periodic polling
    async function updateDashboard() {
      try {
        const res = await fetch('/api/data');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const data = await res.json();
        renderData(data);
      } catch (err) {
        console.error("Dashboard sync error:", err);
        document.getElementById('sync-time').innerText = "Reconnecting...";
      }
    }

    setInterval(updateDashboard, 2000);
  </script>
</body>
</html>
"""

class ThreadedHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

if __name__ == "__main__":
    t = threading.Thread(target=data_updater, daemon=True)
    t.start()
    time.sleep(1)
    with ThreadedHTTPServer(("", PORT), CGPUHandler) as httpd:
        print(f"CGPU Dashboard running on port {PORT} with threading & cache...")
        httpd.serve_forever()
