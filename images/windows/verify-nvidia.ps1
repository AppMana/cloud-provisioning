# Run after an explicit image-builder reboot. Does not install or download software.
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$root='C:\ProgramData\CloudProvisioning'
$record=[IO.File]::ReadAllText((Join-Path $root 'nvidia-image.json')) | ConvertFrom-Json
if ($record.windowsBuild -ne [Environment]::OSVersion.Version.Build) { throw 'GPU recipe OS build mismatch' }
if ($record.installerExitCode -notin @(0,1)) { throw 'GPU installation did not finish successfully' }
$boot=(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')
if ($boot -eq $record.installationBootTime) { throw 'Reboot the image builder before GPU verification' }
$smi=Join-Path $env:SystemRoot 'System32\nvidia-smi.exe'
if (!(Test-Path $smi)) { $smi='C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe' }
if (!(Test-Path $smi)) { throw 'nvidia-smi is missing' }
$output=(& $smi --query-gpu=driver_version,driver_model.current,name --format=csv,noheader | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or !$output) { throw 'NVIDIA enumeration failed' }
$gpus=@($output -split '\r?\n' | ConvertFrom-Csv -Header driverVersion,driverModel,name)
foreach ($gpu in $gpus) {
    if ($gpu.driverVersion.Trim() -ne $record.driverVersion -or $gpu.driverModel.Trim() -ne 'WDDM') { throw 'Expected the pinned driver in WDDM mode' }
}
$devices=@(Get-CimInstance Win32_VideoController | Where-Object { $_.PNPDeviceID -like 'PCI\VEN_10DE*' })
if ($devices.Count -eq 0 -or @($devices | Where-Object ConfigManagerErrorCode -ne 0).Count) { throw 'NVIDIA device reports an error' }
$verification=@{
    windowsBuild=$record.windowsBuild;driverVersion=$record.driverVersion
    packageSHA256=$record.packageSHA256;licenseScope=$record.licenseScope;licenseSource=$record.licenseSource
    bootTime=$boot;verifiedAfterReboot=$true;gpuCount=$gpus.Count;gpus=$gpus
    workloadValidated=$false;licenseActivationValidated=$false
}
$verification | ConvertTo-Json -Depth 5 | Set-Content -Encoding UTF8 (Join-Path $root 'nvidia-verification.json')
$verification | ConvertTo-Json -Depth 5 -Compress
