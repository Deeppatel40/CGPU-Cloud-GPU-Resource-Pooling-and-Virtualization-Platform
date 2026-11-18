@echo off
title Setup and Start Windows OpenSSH Server

:: Check for administrative privileges
net session >nul 2>&1
if %errorlevel% neq 0 (
    echo Administrative privileges required. Prompting for elevation...
    powershell -NoProfile -Command "Start-Process cmd -ArgumentList '/c \"\"%~f0\"\"' -Verb RunAs"
    exit /b
)

echo ========================================================
echo        Configuring Windows OpenSSH Server
echo ========================================================
echo.

powershell -NoProfile -ExecutionPolicy Bypass -Command ^
    "Write-Host '[1/4] Checking OpenSSH Server capability...' -ForegroundColor Cyan; " ^
    "$cap = Get-WindowsCapability -Online | Where-Object Name -like 'OpenSSH.Server*'; " ^
    "if ($cap.State -ne 'Installed') { " ^
    "    Write-Host 'Installing OpenSSH Server (this may take a minute)...' -ForegroundColor Yellow; " ^
    "    Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 | Out-Null; " ^
    "    Write-Host 'Installation completed.' -ForegroundColor Green; " ^
    "} else { " ^
    "    Write-Host 'OpenSSH Server is already installed.' -ForegroundColor Green; " ^
    "}; " ^
    "Write-Host '[2/4] Setting sshd service to Automatic...' -ForegroundColor Cyan; " ^
    "Set-Service -Name sshd -StartupType Automatic; " ^
    "Write-Host '[3/4] Starting sshd service...' -ForegroundColor Cyan; " ^
    "Start-Service sshd; " ^
    "Write-Host '[4/4] Ensuring firewall allows inbound TCP 22...' -ForegroundColor Cyan; " ^
    "if (!(Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Enabled)) { " ^
    "    New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22 | Out-Null; " ^
    "}; " ^
    "Write-Host ''; " ^
    "Write-Host '========================================================' -ForegroundColor Green; " ^
    "Write-Host '  SSH Server is now RUNNING on this PC!' -ForegroundColor Green; " ^
    "Write-Host '  Connect using: ssh deepp@10.1.96.45' -ForegroundColor Green; " ^
    "Write-Host '========================================================' -ForegroundColor Green; "

echo.
pause
