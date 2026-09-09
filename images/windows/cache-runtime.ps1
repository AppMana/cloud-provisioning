# Shared standalone runtime lifecycle for cache building and verification.
function Start-ImageCacheRuntime([string]$Work) {
 $ErrorActionPreference='Stop'
 if(Get-Service k0sworker,cldt-image-cache -ErrorAction SilentlyContinue) { throw 'Worker or cache service already installed' }
 if(Get-Process containerd -ErrorAction SilentlyContinue) { throw 'Runtime already running' }
 if(Test-Path $Work) { throw 'Runtime operation already attempted; inspect existing state' }
 if($Work.Contains("'")) { throw 'Unsupported runtime state path' }
 New-Item -ItemType Directory -Path $Work | Out-Null
 $config=@'
version = 3
root = 'C:\var\lib\k0s\containerd'
disabled_plugins = ['io.containerd.cri.v1.runtime', 'io.containerd.cri.v1.images']
[grpc]
address = '\\.\pipe\cldt-image-cache'
'@
 $config="state = '$Work\state'`n"+$config
 $configPath=Join-Path $Work 'containerd.toml'
 [IO.File]::WriteAllText($configPath,$config,(New-Object Text.UTF8Encoding($false)))
 $exe='C:\var\lib\k0s\bin\containerd.exe'
 & $exe --config $configPath --service-name cldt-image-cache --register-service
 if($LASTEXITCODE -ne 0) { throw 'Cache service registration failed' }
 $handle=@{executable=$exe;config=$configPath}
 try {
  Set-Service cldt-image-cache -StartupType Manual
  Start-Service cldt-image-cache
 } catch {
  Stop-ImageCacheRuntime $handle | Out-Null
  throw
 }
 return $handle
}

function Stop-ImageCacheRuntime($Handle) {
 $ErrorActionPreference='Stop'
 $service=Get-CimInstance Win32_Service | Where-Object Name -eq 'cldt-image-cache'
 if(!$service -or !$service.PathName.Contains($Handle.config)) { throw 'Cache service ownership changed' }
 # Keep a handle to the original service process before requesting shutdown.
 # The SCM can report Stopped before containerd has exited; an immediate
 # name-based query produced a false cleanup failure in the native cache bake.
 $process=$null
 if($service.ProcessId) {
  $process=Get-Process -Id $service.ProcessId -ErrorAction Stop
  if($process.Path -ine $Handle.executable) { throw 'Cache process ownership changed' }
 }
 Stop-Service cldt-image-cache
 if($process -and !$process.WaitForExit(30000)) { throw 'Original cache runtime did not exit within 30 seconds' }
 & $Handle.executable --config $Handle.config --service-name cldt-image-cache --unregister-service
 if($LASTEXITCODE -ne 0) { throw 'Cache service removal failed' }
 return @{serviceStopped= -not [bool](Get-Process containerd -ErrorAction SilentlyContinue);cacheServiceRemoved= -not [bool](Get-Service cldt-image-cache -ErrorAction SilentlyContinue)}
}
