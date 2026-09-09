$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$root = 'C:\ProgramData\CloudProvisioning'
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Image has an installed worker service' }
$record = Get-Content -Raw (Join-Path $root 'image-preparation.json') | ConvertFrom-Json
$build = [Environment]::OSVersion.Version.Build
if ($build -notin @(20348,26100) -or $record.windowsBuild -ne $build) { throw 'Windows image build mismatch' }
if ((Get-WindowsFeature Containers).InstallState -ne 'Installed') { throw 'Containers feature is not installed' }
if (Test-Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending') { throw 'Image still requires a reboot' }
$hash = (Get-FileHash (Join-Path $root 'k0s.exe') -Algorithm SHA256).Hash.ToLowerInvariant()
if ($hash -ne $record.k0sSHA256) { throw 'k0s binary hash changed' }
$version = & (Join-Path $root 'k0s.exe') version
if ($LASTEXITCODE -ne 0 -or $version.Trim() -ne $record.k0s) { throw 'k0s binary version changed' }
foreach ($path in @('C:\var\lib\k0s\pki', 'C:\var\lib\k0s\kubelet.conf', 'C:\k\config', 'C:\k0s-token.txt')) {
    if (Test-Path $path) { throw "Image contains cluster identity: $path" }
}
@{windowsBuild=$build; k0s=$version.Trim(); k0sSHA256=$hash; containersInstalled=$true; pendingReboot=$false; joined=$false} | ConvertTo-Json -Compress
