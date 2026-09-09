# Verify an existing cache; never downloads or imports an image.
param([Parameter(Mandatory=$true)][string]$Recipe)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'cache-runtime.ps1')
$recipeObject=Get-Content -Raw -LiteralPath $Recipe | ConvertFrom-Json
if($recipeObject.schemaVersion -notin @(2,3) -or $recipeObject.dataDirectoryPermissions -ne 'inherit-parent') { throw 'Regenerate the cache recipe for the inherited-permissions layout' }
$root='C:\ProgramData\CloudProvisioning'
$runtime='C:\var\lib\k0s\bin'
$work=Join-Path $PSScriptRoot 'cache-verification'
if(Get-Service k0sworker,cldt-image-cache -ErrorAction SilentlyContinue) { throw 'Worker or cache service already installed' }
if(Get-Process containerd -ErrorAction SilentlyContinue) { throw 'Runtime already running' }
if($recipeObject.windowsBuild -ne [Environment]::OSVersion.Version.Build) { throw 'Cache OS mismatch' }
if((Get-FileHash (Join-Path $root 'k0s.exe')).Hash.ToLowerInvariant() -ne $recipeObject.runtime.workerSHA256) { throw 'Cache worker mismatch' }
foreach($component in $recipeObject.runtime.components) {
 if($component.name -notin @('containerd.exe','containerd-shim-runhcs-v1.exe')) { throw 'Unexpected runtime component' }
 if((Get-FileHash (Join-Path $runtime $component.name)).Hash.ToLowerInvariant() -ne $component.sha256) { throw 'Cache runtime mismatch' }
}
if(!(Test-Path 'C:\var\lib\k0s\containerd')) { throw 'Prepared cache is missing' }
if((Get-Acl -LiteralPath 'C:\var\lib\k0s').AreAccessRulesProtected) { throw 'Cache data-directory ACL prevents normal k0s config inheritance; rebuild the image' }
foreach($path in @('C:\var\lib\k0s\pki','C:\var\lib\k0s\kubelet.conf','C:\k\config','C:\k0s-token.txt')) { if(Test-Path $path) { throw 'Cluster identity present' } }
$handle=Start-ImageCacheRuntime $work
$snapshot=$null
try {
 $snapshot=& (Join-Path $PSScriptRoot 'inspect-cache.ps1') | ConvertFrom-Json
} finally {
 $shutdown=Stop-ImageCacheRuntime $handle
}
$receipt=@{snapshot=$snapshot;serviceStopped=$shutdown.serviceStopped;cacheServiceRemoved=$shutdown.cacheServiceRemoved}
$receipt | ConvertTo-Json -Depth 8 -Compress
