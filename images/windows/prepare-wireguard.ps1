# Provider-independent image layer; the provider adapter handles Sysprep/capture.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$root = 'C:\ProgramData\CloudProvisioning'
if (!(Test-Path $root)) { throw 'Prepare the protected image directory first' }
$ProgressPreference = 'SilentlyContinue'
$archive = Join-Path $root 'wireguard-nt-1.1.zip'
Invoke-WebRequest -UseBasicParsing -Uri 'https://download.wireguard.com/wireguard-nt/wireguard-nt-1.1.zip' -OutFile $archive
if ((Get-FileHash $archive -Algorithm SHA256).Hash.ToLowerInvariant() -ne 'dceb30a9bc4be48cce0f74160fc88a585a2c2627366e8f846fc6658f9038dace') { throw 'WireGuardNT archive checksum mismatch' }
$staging = Join-Path $root 'wireguard-staging'
Expand-Archive -Force $archive $staging
$dll = Join-Path $staging 'wireguard-nt\bin\amd64\wireguard.dll'
if ((Get-FileHash $dll -Algorithm SHA256).Hash.ToLowerInvariant() -ne 'b1b85e072c45d81358be29d94c599dc76652f912be8c0f0a41e2d5d89a6461d3') { throw 'WireGuardNT DLL checksum mismatch' }
$signature = Get-AuthenticodeSignature $dll
if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'CN=WireGuard LLC,') { throw 'WireGuardNT publisher verification failed' }
Copy-Item -Force $dll (Join-Path $root 'wireguard.dll')
Copy-Item -Force (Join-Path $staging 'wireguard-nt\LICENSE.txt') (Join-Path $root 'wireguard-nt-LICENSE.txt')
Remove-Item -Recurse -Force $staging
Remove-Item -Force $archive
# The signed DLL supplies the driver through its documented API. Image capture
# must retain its license alongside the application that uses that API.
