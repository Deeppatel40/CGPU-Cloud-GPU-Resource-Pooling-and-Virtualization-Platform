# setup_sshd.ps1 - Install and start Windows OpenSSH Server
Write-Host "=== Setting up Windows OpenSSH Server ===" -ForegroundColor Cyan

# 1. Install OpenSSH Server
Write-Host "`n[1/3] Installing OpenSSH.Server capability..." -ForegroundColor Yellow
$capability = Get-WindowsCapability -Online | Where-Object Name -like 'OpenSSH.Server*'
if ($capability.State -ne "Installed") {
    Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
    Write-Host "OpenSSH Server successfully installed." -ForegroundColor Green
} else {
    Write-Host "OpenSSH Server is already installed." -ForegroundColor Green
}

# 2. Start and configure sshd service
Write-Host "`n[2/3] Configuring and starting sshd service..." -ForegroundColor Yellow
Set-Service -Name sshd -StartupType 'Automatic'
Start-Service sshd
Write-Host "sshd service status:" -ForegroundColor Green
Get-Service sshd | Format-Table Name, Status, StartType

# 3. Configure firewall rule for port 22
Write-Host "`n[3/3] Checking firewall rules..." -ForegroundColor Yellow
$rule = Get-NetFirewallRule -Name "OpenSSH-Server-In-TCP" -ErrorAction SilentlyContinue
if (-not $rule) {
    New-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -DisplayName 'OpenSSH Server (sshd)' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 22
    Write-Host "Firewall rule created for port 22." -ForegroundColor Green
} else {
    Enable-NetFirewallRule -Name "OpenSSH-Server-In-TCP"
    Write-Host "Firewall rule enabled for port 22." -ForegroundColor Green
}

Write-Host "`n=== Setup Complete! OpenSSH Server is running on port 22 ===" -ForegroundColor Green
Start-Sleep -Seconds 3
