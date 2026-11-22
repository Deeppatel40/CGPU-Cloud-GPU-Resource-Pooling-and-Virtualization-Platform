# =============================================================================
#  start-cgpu.ps1 - start everything CGPU runs on this laptop, in one command
# =============================================================================
#
#   .\start-cgpu.ps1            start everything and print the URLs
#   .\start-cgpu.ps1 -Status    show what is up, change nothing
#   .\start-cgpu.ps1 -Stop      stop what this script started
#   .\start-cgpu.ps1 -Load      start it, then put real load on the GPU
#
# This machine is the RTX 4050 GPU worker. The control plane (K3s server,
# scheduler) runs on PC1 and the API on PC2, so this script does not touch
# those - it starts the parts that live here:
#
#   k3s-agent          joins the cluster so jobs can be scheduled here
#   /opt/cgpu guard    libcgpu_guard.so, or GPU pods fail to mount it
#   cgpu-worker-agent  :9400  GPU + per-job telemetry for Prometheus
#   Prometheus         :9090  scrapes the agent over loopback
#   Grafana            :3001  CGPU dashboard, admin/admin
#   portal             :3100  local copy of the web portal
#
# The Linux side is delegated to scripts/cgpu-worker-stack.sh rather than
# inlined: PowerShell mangles double quotes when passing a command string to a
# native program, which silently truncated the bash mid-`if`. This file handles
# only what is genuinely Windows-side - the portal and the WSL keep-alive.
#
# Everything is idempotent, so re-running it is safe.
#
# For a permanent fix to services dying when WSL idles out, install the systemd
# units instead and this script is only needed for the portal:
#
#   wsl -d Ubuntu -u root
#   cp deploy/systemd/cgpu-*.service /etc/systemd/system/
#   systemctl daemon-reload
#   systemctl enable --now cgpu-worker-agent cgpu-prometheus cgpu-grafana

param(
    [switch]$Status,
    [switch]$Stop,
    [switch]$Load
)

$ErrorActionPreference = 'Continue'
$Repo       = $PSScriptRoot
$StackSh    = '/mnt/d/Cloud_Project/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/scripts/cgpu-worker-stack.sh'
$ApiUrl     = 'http://10.20.24.32:8080'      # cgpu-api on PC2
$PromUrl    = 'http://127.0.0.1:9090'        # node-local Prometheus
$PortalPort = 3100

function Say($msg, $colour = 'Gray') { Write-Host $msg -ForegroundColor $colour }
function Head($msg) { Write-Host ''; Write-Host "== $msg" -ForegroundColor Cyan }

# Calls wsl.exe explicitly and is NOT named Wsl: a function by that name shadows
# the wsl command, so its body calls itself and the script dies with "call depth
# overflow", which points nowhere near the cause.
function Stack($action) { wsl.exe -d Ubuntu -u root --exec bash $StackSh $action }

function PortUp([int]$port) {
    return (Test-NetConnection -ComputerName 127.0.0.1 -Port $port `
                -WarningAction SilentlyContinue -InformationLevel Quiet)
}

function HttpCode($url) {
    try { return (Invoke-WebRequest -Uri $url -TimeoutSec 6 -UseBasicParsing -ErrorAction Stop).StatusCode }
    catch { if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode } else { return 0 } }
}

function Show-Portal {
    $code = HttpCode "http://127.0.0.1:$PortalPort/"
    $state = if ($code -ge 200 -and $code -lt 400) { "up   (HTTP $code)" } else { 'down' }
    Say ("  {0,-16} :{1,-5} {2}" -f 'portal', $PortalPort, $state)
    $api = HttpCode "$ApiUrl/healthz"
    Say ("  {0,-16} {1}" -f 'PC2 API', $(if ($api -eq 200) { "reachable  $ApiUrl" } else { "UNREACHABLE  $ApiUrl" }))
}

function Start-Portal {
    Head "Portal (:$PortalPort)"
    if (PortUp $PortalPort) { Say '  already listening'; return }

    Push-Location (Join-Path $Repo 'portal')
    try {
        if (-not (Test-Path 'node_modules')) {
            Say '  installing dependencies (first run only, a few minutes)'
            cmd /c "npm install --no-audit --no-fund" 2>&1 | Select-Object -Last 1 | ForEach-Object { Say "  $_" }
        }
        # CGPU_API_URL is read at BUILD time - Next.js bakes the /api/v1 rewrite
        # into the build output - so it must be set for the build, not just for
        # `next start`. It also has to be a real environment variable: npm's
        # Windows shim drops inline VAR=value assignments.
        $env:CGPU_API_URL = $ApiUrl
        $env:CGPU_PROMETHEUS_URL = $PromUrl
        Say "  building against API $ApiUrl"
        cmd /c "npx next build" 2>&1 | Select-String -Pattern 'Compiled|error|Error' |
            Select-Object -First 3 | ForEach-Object { Say "  $_" }

        # Detached through cmd /c start so the server survives this script exiting;
        # Start-Process on npx alone leaves a shim that dies with the parent.
        cmd /c "start `"cgpu-portal`" /min cmd /c `"npx next start -p $PortalPort -H 0.0.0.0`""
        for ($i = 0; $i -lt 15; $i++) { Start-Sleep -Seconds 2; if (PortUp $PortalPort) { break } }
        if (PortUp $PortalPort) { Say '  started' } else { Say '  did not come up - run `npx next start` in portal/ to see why' }
    } finally { Pop-Location }
}

function Stop-Portal {
    $pids = (netstat -ano | Select-String ":$PortalPort\s.*LISTENING") |
            ForEach-Object { ($_ -split '\s+')[-1] } | Sort-Object -Unique
    foreach ($p in $pids) { taskkill /PID $p /F 2>$null | Out-Null; Say "  portal stopped (pid $p)" }
    if (-not $pids) { Say '  portal was not running' }
}

function Hold-Wsl {
    # A held-open session stops the distro idling out, which otherwise takes k3s,
    # the agent and both monitoring containers down with it - and looks like a
    # broken cluster rather than an idle timeout.
    $held = Get-CimInstance Win32_Process -Filter "Name='wsl.exe'" -ErrorAction SilentlyContinue |
            Where-Object { $_.CommandLine -like '*sleep infinity*' }
    if (-not $held) {
        Start-Process -FilePath 'wsl.exe' -ArgumentList @('-d', 'Ubuntu', '--exec', 'sleep', 'infinity') -WindowStyle Hidden
        Say '  holding a WSL session open so the distro does not idle out' 'DarkGray'
    }
}

# ------------------------------------------------------------------ main ------
Say ''
Say 'CGPU - RTX 4050 GPU worker' 'Green'

if ($Stop) {
    Head 'Stopping'
    Stop-Portal
    Stack 'stop'
    Say ''
    exit 0
}

if ($Status) {
    Stack 'status'
    Show-Portal
    Say ''
    exit 0
}

Stack 'start'
Start-Portal
Hold-Wsl
if ($Load) { Stack 'load' }

Head 'Status'
Stack 'status'
Show-Portal

Head 'Open these'
Say "  Portal       http://localhost:$PortalPort" 'White'
Say '  Grafana      http://localhost:3001        (admin / admin)' 'White'
Say '  Prometheus   http://localhost:9090' 'White'
Say '  Team portal  http://10.20.24.32:3000      (runs on PC2)' 'White'
Say ''
Say '  Submit a job: Portal -> Submit job -> "GPU matmul benchmark"' 'DarkGray'
Say '  Load the GPU: .\start-cgpu.ps1 -Load' 'DarkGray'
Say '  Stop again:   .\start-cgpu.ps1 -Stop' 'DarkGray'
Say ''
