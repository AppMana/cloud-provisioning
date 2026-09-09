# Read-only inspection of the standalone k0s image-build runtime.
# This does not prepare images, stop the runtime, or qualify a cloned VM.
$ErrorActionPreference='Stop'
$worker='C:\ProgramData\CloudProvisioning\k0s.exe'
$pipe='\\.\pipe\cldt-image-cache'
function Invoke-CtrLines([string[]]$Arguments) {
 $lines=@(& $worker ctr --address $pipe --namespace k8s.io @Arguments)
 if($LASTEXITCODE -ne 0) { throw 'Cache inspection command failed' }
 return @($lines | Where-Object { $_.Trim() })
}
$ready=@(Invoke-CtrLines @('images','check','--quiet','--snapshotter','windows'))
$images=@(Invoke-CtrLines @('images','list','--quiet'))
$targets=@{}
# containerd 2.3.2 inspect prints a display tree. The first three list columns
# contain name, media type and target digest; human-readable size may have spaces.
$metadata=@(Invoke-CtrLines @('images','list'))
if($metadata.Count -eq 0 -or $metadata[0] -notmatch '^REF\s+TYPE\s+DIGEST\s') { throw 'Unexpected image metadata header' }
foreach($row in @($metadata | Select-Object -Skip 1)) {
 $columns=$row.Trim() -split '\s+',4
 if($columns.Count -ne 4 -or $columns[2] -cnotmatch '^sha256:[a-f0-9]{64}$' -or $targets.ContainsKey($columns[0])) { throw 'Ambiguous image metadata row' }
 $targets[$columns[0]]=$columns[2]
}
if($targets.Count -ne $images.Count -or @($images | Where-Object {!$targets.ContainsKey($_)}).Count) { throw 'Image references changed during inspection' }
$containers=@(Invoke-CtrLines @('containers','list','--quiet'))
$tasks=@(Invoke-CtrLines @('tasks','list','--quiet'))
$record=@{
 observedAt=[DateTime]::UtcNow.ToString('o')
 windowsBuild=[Environment]::OSVersion.Version.Build
 pipe=$pipe
 namespace='k8s.io'
 root='C:\var\lib\k0s\containerd'
 snapshotter='windows'
 dataDirectoryInheritsPermissions= -not (Get-Acl -LiteralPath 'C:\var\lib\k0s').AreAccessRulesProtected
 readyImages=$ready
 registeredImages=$images
 imageTargets=$targets
 containers=$containers
 tasks=$tasks
 workerServicePresent=[bool](Get-Service k0sworker -ErrorAction SilentlyContinue)
 identityPathsPresent=@(@('C:\var\lib\k0s\pki','C:\var\lib\k0s\kubelet.conf','C:\k\config','C:\k0s-token.txt') | Where-Object {Test-Path $_})
 cacheService=(Get-Service cldt-image-cache).Status.ToString()
 workerSHA256=(Get-FileHash $worker).Hash.ToLowerInvariant()
 containerdSHA256=(Get-FileHash 'C:\var\lib\k0s\bin\containerd.exe').Hash.ToLowerInvariant()
 shimSHA256=(Get-FileHash 'C:\var\lib\k0s\bin\containerd-shim-runhcs-v1.exe').Hash.ToLowerInvariant()
}
$record | ConvertTo-Json -Depth 5 -Compress
