$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$plan = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__PLAN_BASE64__')) | ConvertFrom-Json
$root = 'C:\ProgramData\CloudProvisioning'
$mesh = Join-Path $root $plan.interface
$k0s = Join-Path $root 'k0s.exe'
$tunnel = Join-Path $root 'windows-tunnel.exe'
if ([Environment]::OSVersion.Version.Build -notin @(20348,26100)) { throw 'Windows image build is unsupported' }
if (!(Get-Acl $root).AreAccessRulesProtected) { throw 'Image directory must have a protected ACL' }
if ((Get-WindowsFeature Containers).InstallState -ne 'Installed') { throw 'Containers must be enabled in the image' }
if ((Get-FileHash $tunnel -Algorithm SHA256).Hash -ne $plan.serviceSHA256) { throw 'Baked tunnel executable checksum mismatch' }
if ((Get-AuthenticodeSignature (Join-Path $root 'wireguard.dll')).Status -ne 'Valid') { throw 'WireGuardNT signature invalid' }
$version = & $k0s version
if ($LASTEXITCODE -ne 0 -or $version.Trim() -ne $plan.k0sVersion) { throw 'Baked k0s version differs from expected worker version' }
if ($plan.workerSHA256 -and (Get-FileHash $k0s -Algorithm SHA256).Hash -ne $plan.workerSHA256) { throw 'Baked k0s executable checksum mismatch' }
if (Get-Service k0sworker -ErrorAction SilentlyContinue) { throw 'Refusing to overwrite an installed worker' }
if (Test-Path $mesh) { throw 'Refusing to overwrite an existing mesh identity' }
# Every provider owns its runtime discovery program. VM infrastructure supplies
# already observed static values and does not query a cloud metadata endpoint.
$providerID = $plan.providerID
$nodeAddress = $plan.nodeAddress
if ($plan.identityPowerShell) {
    $script = [ScriptBlock]::Create([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($plan.identityPowerShell)))
    $identity = & $script
    if (!$providerID) { $providerID = $identity.providerID }
    if (!$nodeAddress) { $nodeAddress = $identity.nodeAddress }
}
if (!$providerID -or !$nodeAddress) { throw 'Infrastructure identity discovery failed' }
New-Item -ItemType Directory -Path $mesh | Out-Null
$utf8 = [Text.UTF8Encoding]::new($false)
[IO.File]::WriteAllText((Join-Path $mesh 'peers.json'),($plan.peers | ConvertTo-Json -Depth 20 -Compress),$utf8)
[IO.File]::WriteAllText((Join-Path $mesh 'machine-name'),$plan.machineName,$utf8)
$tokenFile = Join-Path $mesh 'join-token'
[IO.File]::WriteAllText($tokenFile,$plan.joinToken,$utf8)
$name = 'cloud-provisioning-' + $plan.interface
$command = '"'+$tunnel+'" --service-name '+$name+' --interface '+$plan.interface+' --listen-port '+$plan.listenPort+' --peers-file "'+$mesh+'\peers.json" --updates-file "'+$mesh+'\updates.json" --cache-file "'+$mesh+'\cache.json" --request-file "'+$mesh+'\request.json" --receipt-file "'+$mesh+'\receipt.json"'
New-Service -Name $name -BinaryPathName $command -StartupType Automatic | Out-Null
& sc.exe failure $name reset= 86400 actions= restart/5000/restart/10000/restart/30000 | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Setting tunnel service recovery failed' }
New-NetFirewallRule -Name ($name+'-udp') -DisplayName ($name+'-udp') -Direction Inbound -Action Allow -Protocol UDP -LocalPort $plan.listenPort | Out-Null
Start-Service $name
(Get-Service $name).WaitForStatus('Running',[TimeSpan]::FromSeconds(60))
# k0s owns its distribution-default runtime, CNI installation and load balancer.
# Pass one string as kubelet-extra-args; no shell evaluates the supplied values.
$extra = ($plan.kubeletExtraArgs+' --node-ip='+$nodeAddress+' --provider-id='+$providerID+' --hostname-override='+$plan.machineName).Trim()
& $k0s install worker --token-file $tokenFile --kubelet-extra-args $extra
if ($LASTEXITCODE -ne 0) { throw 'k0s worker service installation failed' }
& $k0s start
if ($LASTEXITCODE -ne 0) { throw 'k0s worker start failed' }
# This marks service installation only. CAPI readiness requires a registered
# Node, matching provider ID, and the separate live networking matrix.
[IO.File]::WriteAllText((Join-Path $mesh 'bootstrap-installed'),"installed`n",$utf8)
