# Common Windows image layer. The cloud adapter supplies a licensed local package.
param(
    [Parameter(Mandatory=$true)][string]$Source,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$SHA256,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9]{3}\.[0-9]{2}$')][string]$DriverVersion,
    [Parameter(Mandatory=$true)][ValidateSet('aws','gce','azure','byol')][string]$LicenseScope,
    [Parameter(Mandatory=$true)][string]$LicenseSource
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$root='C:\ProgramData\CloudProvisioning'
if ([Environment]::OSVersion.Version.Build -notin @(20348,26100)) { throw 'Unsupported Windows build' }
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Cannot install an image driver on a joined worker' }
foreach ($path in @('C:\var\lib\k0s\pki','C:\var\lib\k0s\kubelet.conf','C:\k\config','C:\k0s-token.txt')) {
    if (Test-Path $path) { throw 'Cannot bake cluster identity' }
}
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Prepare the protected image directory first' }
if ((Get-Item $root).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Image directory is a reparse point' }
if ([string]::IsNullOrWhiteSpace($LicenseSource)) { throw 'Record the package license source' }
# Before installation, the GPU can be an unclassified 3D controller (code 28)
# and absent from Win32_VideoController. Inspect PCI inventory instead.
$devices=@(Get-CimInstance Win32_PnPEntity | Where-Object { $_.PNPDeviceID -like 'PCI\VEN_10DE*' })
if ($devices.Count -eq 0) { throw 'Image preparation requires an NVIDIA GPU VM' }
if ((Get-FileHash -LiteralPath $Source -Algorithm SHA256).Hash -ne $SHA256) { throw 'Driver package checksum mismatch' }
$signature=Get-AuthenticodeSignature -LiteralPath $Source
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'NVIDIA Corporation') { throw 'Driver package lacks a valid NVIDIA signature' }
$record=@{
    windowsBuild=[Environment]::OSVersion.Version.Build
    driverVersion=$DriverVersion
    packageSHA256=$SHA256.ToLowerInvariant()
    licenseScope=$LicenseScope
    licenseSource=$LicenseSource
    signer=$signature.SignerCertificate.Subject
    installationBootTime=(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')
    verifiedAfterReboot=$false
}
# A failed or interrupted installation must not retain a previous acceptance receipt.
Remove-Item -LiteralPath (Join-Path $root 'nvidia-verification.json') -Force -ErrorAction SilentlyContinue
$record | ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $root 'nvidia-image.json')
$process=Start-Process -FilePath $Source -ArgumentList '-s','-n' -Wait -PassThru
# NVIDIA reports 1 for successful installation requiring a reboot.
if ($process.ExitCode -notin @(0,1)) { throw "NVIDIA installer failed with exit code $($process.ExitCode)" }
$record.installerExitCode=$process.ExitCode
$record | ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $root 'nvidia-image.json')
$record | ConvertTo-Json -Compress
# The adapter reboots, runs verify-nvidia.ps1, then generalizes and captures.
