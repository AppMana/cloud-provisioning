# Run as Administrator/SYSTEM while building a fresh, unjoined Windows image.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$build = [Environment]::OSVersion.Version.Build
if ($build -notin @(20348, 26100)) { throw "Unsupported Windows build $build" }
foreach ($path in @('C:\var\lib\k0s\pki', 'C:\var\lib\k0s\kubelet.conf', 'C:\k\config', 'C:\k0s-token.txt')) {
    if (Test-Path $path) { throw "Refusing to bake cluster identity: $path" }
}
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Refusing to prepare an installed worker' }
$root = 'C:\ProgramData\CloudProvisioning'
$null = New-Item -ItemType Directory -Force -Path $root
$acl = New-Object System.Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($sid in @('S-1-5-18', 'S-1-5-32-544')) {
    $identity = New-Object System.Security.Principal.SecurityIdentifier($sid)
    $rule = New-Object System.Security.AccessControl.FileSystemAccessRule($identity, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
    $acl.AddAccessRule($rule)
}
Set-Acl -Path $root -AclObject $acl
$version = 'v1.36.2+k0s.0'
$sha256 = '81d05aec71b1d34a8d8ae73641293c8b2f45abf9aaa2d55e332ed41103654c65'
$binary = Join-Path $root 'k0s.exe'
if (!(Test-Path $binary) -or (Get-FileHash $binary -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sha256) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $ProgressPreference = 'SilentlyContinue'
    Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/k0sproject/k0s/releases/download/v1.36.2%2Bk0s.0/k0s-v1.36.2%2Bk0s.0-amd64.exe' -OutFile ($binary + '.download')
    if ((Get-FileHash ($binary + '.download') -Algorithm SHA256).Hash.ToLowerInvariant() -ne $sha256) { throw 'k0s checksum mismatch' }
    Move-Item -Force ($binary + '.download') $binary
}
$actual = & $binary version
if ($LASTEXITCODE -ne 0 -or $actual.Trim() -ne $version) { throw "k0s version mismatch: $actual" }
$feature = Install-WindowsFeature -Name Containers
if (!$feature.Success) { throw 'Containers feature installation failed' }
@{ windowsBuild=$build; k0s=$version; k0sSHA256=$sha256; containersInstalled=$true; rebootRequired=($feature.RestartNeeded.ToString() -eq 'Yes') } |
    ConvertTo-Json | Set-Content -Encoding UTF8 (Join-Path $root 'image-preparation.json')
# The image adapter reboots and verifies before Sysprep/capture. Do not join a
# cluster or bake its token, node certificate, WireGuard key or peer state.
