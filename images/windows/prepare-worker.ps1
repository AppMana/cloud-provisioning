# Replace the worker artifact on an unjoined image builder; no network download.
param(
    [Parameter(Mandatory=$true)][string]$Source,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$SHA256,
    [Parameter(Mandatory=$true)][ValidatePattern('^v[0-9]+\.[0-9]+\.[0-9]+\+k0s\.[a-zA-Z0-9.+-]+$')][string]$Version
)
$ErrorActionPreference='Stop'
$root='C:\ProgramData\CloudProvisioning'
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Prepare the protected image directory first' }
if ((Get-Item $root).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Image directory is a reparse point' }
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Cannot bake an installed worker' }
foreach ($path in @('C:\var\lib\k0s\pki','C:\var\lib\k0s\kubelet.conf','C:\k\config','C:\k0s-token.txt')) {
    if (Test-Path $path) { throw 'Cannot bake cluster identity' }
}
$recordPath=Join-Path $root 'image-preparation.json'
$record=[IO.File]::ReadAllText($recordPath) | ConvertFrom-Json
if ($record.windowsBuild -ne [Environment]::OSVersion.Version.Build -or !$record.containersInstalled) { throw 'Prepare the Windows base image first' }
if ((Get-FileHash $Source -Algorithm SHA256).Hash -ne $SHA256) { throw 'Worker artifact checksum mismatch' }
$actual=(& $Source version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $actual -ne $Version) { throw 'Worker artifact version mismatch' }
$destination=Join-Path $root 'k0s.exe'
Copy-Item -LiteralPath $Source -Destination $destination -Force
if ((Get-FileHash $destination -Algorithm SHA256).Hash -ne $SHA256) { throw 'Staged worker checksum mismatch' }
$record.k0s=$Version
$record.k0sSHA256=$SHA256.ToLowerInvariant()
$record | ConvertTo-Json | Set-Content -Encoding UTF8 ($recordPath+'.candidate')
Move-Item -LiteralPath ($recordPath+'.candidate') -Destination $recordPath -Force
