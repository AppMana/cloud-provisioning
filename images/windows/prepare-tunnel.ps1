# Stage a locally built artifact during image baking; no network installation.
param(
    [Parameter(Mandatory=$true)][string]$Source,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-fA-F0-9]{64}$')][string]$SHA256
)
$ErrorActionPreference='Stop'
$root='C:\ProgramData\CloudProvisioning'
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Prepare the protected image directory first' }
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Cannot bake an installed worker' }
if ((Get-FileHash $Source -Algorithm SHA256).Hash -ne $SHA256) { throw 'Tunnel artifact checksum mismatch' }
if (!(Test-Path (Join-Path $root 'wireguard.dll'))) { throw 'Prepare WireGuardNT before staging the service' }
Copy-Item -Force $Source (Join-Path $root 'windows-tunnel.exe')
@{tunnelSHA256=$SHA256.ToLowerInvariant()} | ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $root 'tunnel-image.json')
