# Pre-unpack pinned workload images on an unjoined VM, before cloud capture.
param(
 [string]$RuntimeArchive,
 [string]$RuntimeManifest,
 [Parameter(Mandatory=$true)][string]$Recipe,
 [Parameter(Mandatory=$true)][string]$UnpackHelper,
 [Parameter(Mandatory=$true)][ValidatePattern('^[a-f0-9]{64}$')][string]$UnpackHelperSHA256,
 [string]$PreviousRecipe,
 [string]$PullAuth,
 [ValidateRange(0,4000)][int]$MinimumFreeGiB=0,
 [ValidatePattern('^[a-z0-9][a-z0-9-]{0,60}$')][string]$OperationName='cache-build'
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'cache-runtime.ps1')
$root='C:\ProgramData\CloudProvisioning'
$work=Join-Path $root $OperationName
$data='C:\var\lib\k0s'
$worker=Join-Path $root 'k0s.exe'
$extending=![string]::IsNullOrEmpty($PreviousRecipe)
if((Get-FileHash -LiteralPath $UnpackHelper).Hash.ToLowerInvariant() -cne $UnpackHelperSHA256) { throw 'Native unpack helper identity mismatch' }
$manifest=$null
if(!$extending) {
 if(!$RuntimeArchive -or !$RuntimeManifest) { throw 'Fresh preparation requires runtime archive and manifest' }
 $manifest=Get-Content -Raw -LiteralPath $RuntimeManifest | ConvertFrom-Json
}
$recipeObject=Get-Content -Raw -LiteralPath $Recipe | ConvertFrom-Json
if($recipeObject.schemaVersion -notin @(2,3) -or $recipeObject.dataDirectoryPermissions -ne 'inherit-parent') { throw 'Regenerate the cache recipe for the inherited-permissions layout' }
if(!(Get-Acl $root).AreAccessRulesProtected -or ((Get-Item $root).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Unprotected image directory' }
if(Get-Service k0sworker,cldt-image-cache -ErrorAction SilentlyContinue) { throw 'Worker or cache service already installed' }
if(Get-Process containerd -ErrorAction SilentlyContinue) { throw 'Runtime already running' }
if((Test-Path $data) -ne $extending) { throw 'Fresh preparation requires absent runtime data; extension requires the existing cache' }
if(Test-Path (Join-Path $work 'intent.json')) { throw 'Cache preparation already requested; inspect its receipt' }
if(@(Get-NetAdapter -Physical).Count -ne 1) { throw 'Expected one physical NIC' }
if((Get-PSDrive C).Free -lt ($MinimumFreeGiB*1GB)) { throw 'Insufficient free space for requested cache build' }
foreach($path in @('C:\k\config','C:\k0s-token.txt')) { if(Test-Path $path) { throw 'Cluster identity present' } }
if(@(Get-ChildItem $root -Directory | Where-Object Name -Match '^cldt[0-9a-f]{8}$').Count) { throw 'Mesh identity present' }
if($recipeObject.windowsBuild -ne [Environment]::OSVersion.Version.Build) { throw 'Cache OS mismatch' }
if((Get-FileHash $worker).Hash.ToLowerInvariant() -ne $recipeObject.runtime.workerSHA256) { throw 'Cache worker mismatch' }
if(!$extending) {
 if($manifest.workerSHA256 -ne $recipeObject.runtime.workerSHA256) { throw 'Runtime manifest worker mismatch' }
 if((Get-FileHash $RuntimeArchive).Hash.ToLowerInvariant() -ne $manifest.runtimeArchiveSHA256) { throw 'Runtime archive mismatch' }
 if(@($manifest.components).Count -ne 2) { throw 'Expected two manifest components' }
}
if(@($recipeObject.runtime.components).Count -ne 2) { throw 'Expected two runtime components' }
foreach($component in $recipeObject.runtime.components) {
 if($component.name -notin @('containerd.exe','containerd-shim-runhcs-v1.exe')) { throw 'Unexpected runtime component' }
 if($extending) {
  if((Get-FileHash (Join-Path (Join-Path $data 'bin') $component.name)).Hash.ToLowerInvariant() -ne $component.sha256) { throw 'Existing runtime mismatch' }
 } else {
  $match=@($manifest.components | Where-Object name -eq $component.name)
  if($match.Count -ne 1 -or $match[0].sha256 -ne $component.sha256) { throw 'Runtime manifests differ' }
 }
}
if(@($recipeObject.images).Count -eq 0) { throw 'No images requested' }
foreach($image in $recipeObject.images) { if($image -cnotmatch '^[a-zA-Z0-9./:_-]+@sha256:[a-f0-9]{64}$') { throw 'Require digest-pinned image references' } }
$aliases=@()
$references=@($recipeObject.images)
if($recipeObject.schemaVersion -eq 3) {
 $aliases=@($recipeObject.aliases)
 if($aliases.Count -eq 0) { throw 'Alias recipe requires explicit aliases' }
 foreach($alias in $aliases) {
  if($alias.reference -cnotmatch '^[a-zA-Z0-9./:_-]+:[a-zA-Z0-9_][a-zA-Z0-9_.-]*$' -or $alias.image -cnotin $recipeObject.images -or $alias.reference -cin $references) { throw 'Invalid or duplicate image alias' }
  $repository=($alias.image -split '@')[0] -replace ':[^/:]+$',''
  if(($alias.reference -replace ':[^/:]+$','') -cne $repository) { throw 'Alias repository mismatch' }
  $references+=@($alias.reference)
 }
}
if(!(Test-Path $work)) { New-Item -ItemType Directory -Path $work | Out-Null }
if((Get-Item $work).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Cache work directory is a reparse point' }
$record=@{unpackHelperSHA256=$UnpackHelperSHA256;recipeSHA256=$recipeObject.sha256;images=@($recipeObject.images);startedAt=[DateTime]::UtcNow.ToString('o');status='running';pulls=@()}
$record | ConvertTo-Json -Depth 8 | Set-Content -Encoding UTF8 (Join-Path $work 'intent.json')
$handle=$null
$hosts=Join-Path $work 'registry-hosts'
try {
 if(!$extending) { New-Item -ItemType Directory -Path $data | Out-Null }
 # Match a normal k0s installation's inherited data-directory permissions.
 # Copying the protected bootstrap ACL here also restricts future nllb config
 # files, which Traefik must read as NT AUTHORITY\Local service.
 # Containerd protects its own store separately.
 if((Get-Acl -LiteralPath $data).AreAccessRulesProtected) { throw 'k0s data directory must retain inherited permissions' }
 if(!$extending) { Expand-Archive -LiteralPath $RuntimeArchive -DestinationPath (Join-Path $data 'bin') }
 foreach($component in $recipeObject.runtime.components) {
  $file=Join-Path (Join-Path $data 'bin') $component.name
  if((Get-FileHash $file).Hash.ToLowerInvariant() -ne $component.sha256) { throw 'Staged runtime mismatch' }
  if(!$extending) { (Get-Item $file).LastWriteTimeUtc=(Get-Item $worker).LastWriteTimeUtc }
 }
 $handle=Start-ImageCacheRuntime (Join-Path $work 'runtime')
 $runtimeReady=$false
 for($attempt=0;$attempt -lt 30;$attempt++) {
  & $worker ctr --address '\\.\pipe\cldt-image-cache' version *> $null
  if($LASTEXITCODE -eq 0) { $runtimeReady=$true;break }
  Start-Sleep 1
 }
 if(!$runtimeReady) { throw 'Original cache runtime did not become available' }
 $existing=@()
 if($extending) {
  $previous=Get-Content -Raw -LiteralPath $PreviousRecipe | ConvertFrom-Json
  if($previous.schemaVersion -notin @(2,3) -or $previous.windowsBuild -ne $recipeObject.windowsBuild -or $previous.runtime.workerSHA256 -ne $recipeObject.runtime.workerSHA256) { throw 'Previous recipe identity mismatch' }
  if(@($previous.runtime.components).Count -ne 2) { throw 'Previous runtime components missing' }
  foreach($component in $recipeObject.runtime.components) {
   $oldComponent=@($previous.runtime.components | Where-Object name -CEQ $component.name)
   if($oldComponent.Count -ne 1 -or $oldComponent[0].sha256 -cne $component.sha256) { throw 'Extension would change runtime identity' }
  }
  $previousReferences=@($previous.images)
  if($previous.schemaVersion -eq 3) { $previousReferences+=@($previous.aliases | ForEach-Object reference) }
  $before=& (Join-Path $PSScriptRoot 'inspect-cache.ps1') | ConvertFrom-Json
  if(@($before.containers).Count -or @($before.tasks).Count -or @($before.identityPathsPresent).Count -or $before.workerServicePresent) { throw 'Existing cache is not unjoined and idle' }
  if(Compare-Object @($before.registeredImages) $previousReferences) { throw 'Existing cache references differ from previous recipe' }
  if(Compare-Object @($before.readyImages) $previousReferences) { throw 'Existing cache is not fully unpacked' }
  foreach($reference in $previousReferences) {
   if($reference -cnotin $references) { throw 'Extension would remove an existing reference' }
   $pinned=$reference
   if($previous.schemaVersion -eq 3) { $oldAlias=@($previous.aliases | Where-Object reference -CEQ $reference); if($oldAlias.Count -eq 1){$pinned=$oldAlias[0].image} }
   $newPinned=$reference
   $newAlias=@($aliases | Where-Object reference -CEQ $reference)
   if($newAlias.Count -eq 1) { $newPinned=$newAlias[0].image }
   if(($newPinned -split '@')[1] -cne ($pinned -split '@')[1]) { throw 'Extension would retarget an existing reference' }
   if($before.imageTargets.$reference -cne ($pinned -split '@')[1]) { throw 'Existing cache target differs from previous recipe' }
  }
  $existing=$previousReferences
  $record.previousSnapshot=$before
 }
 $authArgs=@()
 if($PullAuth) {
  $auth=Get-Content -Raw -LiteralPath $PullAuth | ConvertFrom-Json
  if($auth.registry -cnotmatch '^[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com$' -or $auth.auth -cnotmatch '^[A-Za-z0-9+/]+=*$') { throw 'Invalid build registry credentials' }
  $directory=New-Item -ItemType Directory -Path (Join-Path $hosts $auth.registry)
  $toml='server = "https://'+$auth.registry+'"'+"`n"+'[header]'+"`n"+'authorization = "Basic '+$auth.auth+'"'+"`n"
  [IO.File]::WriteAllText((Join-Path $directory.FullName 'hosts.toml'),$toml,(New-Object Text.UTF8Encoding($false)))
  $auth=$null;$toml=$null
  Remove-Item -LiteralPath $PullAuth -Force
  $authArgs=@('--hosts-dir',$hosts)
 }
 $index=0
 foreach($image in $recipeObject.images) {
  if($image -cin $existing) { continue }
  $started=[DateTime]::UtcNow
  # -Wait retains the native exit code on the tested Windows PowerShell host.
  # A later WaitForExit() on the asynchronous Start-Process object returned null.
  # Use the native Windows platform matcher, including OS version. A bare
  # windows/amd64 override may unpack a different index child than CRI uses.
  $arguments=@('ctr','--address','\\.\pipe\cldt-image-cache','--namespace','k8s.io','images','pull','--local','--snapshotter','windows','--label','io.cri-containerd.pinned=pinned','--label','io.cri-containerd.image=managed')+$authArgs+@($image)
  $process=Start-Process -FilePath $worker -ArgumentList $arguments -Wait -PassThru -NoNewWindow -RedirectStandardOutput (Join-Path $work "pull-$index.stdout.log") -RedirectStandardError (Join-Path $work "pull-$index.stderr.log")
  $record.pulls+=@{image=$image;exitCode=$process.ExitCode;elapsedSeconds=([DateTime]::UtcNow-$started).TotalSeconds}
  if($null -eq $process.ExitCode -or $process.ExitCode -ne 0) { throw 'Image pull failed; inspect guest logs and containerd readiness' }
  $index++
 }
 # ctr local pull wraps its matcher in platforms.Only(), losing Windows
 # version ordering. The helper uses the native default matcher for existing
 # content, exactly as readiness checks do; it never resolves registry tags.
 & $UnpackHelper @($recipeObject.images) > (Join-Path $work 'native-unpack.jsonl')
 if($LASTEXITCODE -ne 0) { throw 'Native platform unpack failed; inspect original operation' }
 foreach($alias in $aliases) {
  if($alias.reference -cin $existing) { continue }
  # Local tagging copies the existing descriptor and labels. Never force an
  # existing reference or ask the registry to resolve a mutable tag.
  & $worker ctr --address '\\.\pipe\cldt-image-cache' --namespace k8s.io images tag --local $alias.image $alias.reference | Out-Null
  if($LASTEXITCODE -ne 0) { throw 'Image alias creation failed; inspect original cache operation' }
 }
 $record.snapshot=& (Join-Path $PSScriptRoot 'inspect-cache.ps1') | ConvertFrom-Json
 if(@($record.snapshot.readyImages).Count -ne $references.Count -or (Compare-Object @($record.snapshot.readyImages) $references)) { throw 'Expected images and aliases are not fully unpacked' }
 foreach($reference in $references) {
  $image=$reference
  $alias=@($aliases | Where-Object reference -CEQ $reference)
  if($alias.Count -eq 1) { $image=$alias[0].image }
  if($record.snapshot.imageTargets.$reference -cne ($image -split '@')[1]) { throw 'Cached reference target does not match pinned recipe' }
 }
 $record.status='prepared'
} catch {
 $record.status='failed'
 $record.error=$_.Exception.Message
} finally {
 try {
  if($PullAuth -and (Test-Path -LiteralPath $PullAuth)) { Remove-Item -LiteralPath $PullAuth -Force }
  if(Test-Path -LiteralPath $hosts) { Remove-Item -LiteralPath $hosts -Recurse -Force }
  $record.registryCredentialsRemoved=(!$PullAuth -or !(Test-Path -LiteralPath $PullAuth)) -and !(Test-Path -LiteralPath $hosts)
 } catch { $record.status='failed';$record.credentialCleanupFailed=$true }
 if($handle) {
  try {
   $shutdown=Stop-ImageCacheRuntime $handle
   $record.serviceStopped=$shutdown.serviceStopped
   $record.cacheServiceRemoved=$shutdown.cacheServiceRemoved
   if(!$shutdown.serviceStopped -or !$shutdown.cacheServiceRemoved) { throw 'Cache runtime still present' }
  } catch { $record.status='failed';$record.shutdownError=$_.Exception.Message }
 }
 $record.finishedAt=[DateTime]::UtcNow.ToString('o')
 $record | ConvertTo-Json -Depth 8 | Set-Content -Encoding UTF8 (Join-Path $work 'result.json')
 $record | ConvertTo-Json -Depth 8 -Compress
}
if($record.status -ne 'prepared') { exit 1 }
