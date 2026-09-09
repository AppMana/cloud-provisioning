#!/usr/bin/env python3
"""Validate native Windows service lifecycle on the run's single-NIC EC2 VMs.

Requires already running, prepared Server 2022/2025 VMs. Does not join a cluster.
Creates and removes a unique manual-start service and its diagnostic artifacts.
"""
import argparse, base64, concurrent.futures, hashlib, json, os, pathlib, subprocess, zipfile
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PrivateFormat, PublicFormat, NoEncryption

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--work-dir',required=True,type=pathlib.Path)
p.add_argument('--awsnode',required=True,type=pathlib.Path)
p.add_argument('--service',required=True,type=pathlib.Path)
p.add_argument('--driver',required=True,type=pathlib.Path)
p.add_argument('--driver-license',required=True,type=pathlib.Path)
p.add_argument('--output',required=True,type=pathlib.Path)
p.add_argument('--coverage',action='store_true',help='require and collect runtime coverage from an instrumented service')
a=p.parse_args();os.umask(0o077)
s=json.loads((a.work_dir/'resources.json').read_text())
if s.get('cleanedUp'):raise RuntimeError('run is cleaned up')
for f in [a.awsnode,a.service,a.driver,a.driver_license]:
 if not f.is_file():raise RuntimeError('build artifact missing')
if hashlib.sha256(a.driver.read_bytes()).hexdigest()!='b1b85e072c45d81358be29d94c599dc76652f912be8c0f0a41e2d5d89a6461d3':raise RuntimeError('unrecognized WireGuardNT DLL')
c=json.loads((a.work_dir/'harness-session.json').read_text())['Credentials']
env=dict(os.environ,AWS_ACCESS_KEY_ID=c['AccessKeyId'],AWS_SECRET_ACCESS_KEY=c['SecretAccessKey'],AWS_SESSION_TOKEN=c['SessionToken'])
ids={y:s['windowsProbeInstances'][y]['instanceID'] for y in ['2022','2025']}
raw=subprocess.check_output(['aws','--region',s['region'],'ec2','describe-instances','--instance-ids',*ids.values(),'--output','json'],env=env)
instances={i['InstanceId']:i for r in json.loads(raw)['Reservations'] for i in r['Instances']}
for id in ids.values():
 i=instances[id]
 if i['VpcId']!=s['vpcID'] or {t['Key']:t['Value'] for t in i['Tags']}.get('cloud-provisioning-test')!=s['runID'] or len(i['NetworkInterfaces'])!=1 or i.get('Platform')!='windows' or i['State']['Name']!='running':raise RuntimeError('instance preflight failed')
a.output.mkdir(parents=True,exist_ok=False)
mesh='cldt'+hashlib.sha256((s['runID']+str(a.output.resolve())).encode()).hexdigest()[:8]
root='C:\\ProgramData\\CloudProvisioning\\'+mesh

def node(year,*args,input=None):
 r=subprocess.run([str(a.awsnode.resolve()),'-work-dir',str(a.work_dir.resolve()),'-instance-id',ids[year],*args],input=input,capture_output=True,timeout=360)
 if r.returncode:raise RuntimeError(year+' guest command failed: '+r.stderr.decode(errors='replace'))
 return r.stdout

def run(year):
 key=X25519PrivateKey.generate();peer=X25519PrivateKey.generate()
 doc={'privateKey':base64.b64encode(key.private_bytes(Encoding.Raw,PrivateFormat.Raw,NoEncryption())).decode(),'localAddress':'10.254.253.1/32','peers':[{'publicKey':base64.b64encode(peer.public_key().public_bytes(Encoding.Raw,PublicFormat.Raw)).decode(),'endpoint':'192.0.2.2:51820','allowedIPs':['10.254.253.2/32','10.244.99.0/24'],'routeHosts':['10.254.253.2']}]}
 script=r'''$ErrorActionPreference='Stop'
$r='ROOT'
$n='MESH'
$expectedBuild=BUILD
$registered=$false
$proxyRegistered=$false
$jobs=@()
$proxyName=$n+'-api'
function Read-API([int]$port) {
 $client=[Net.Sockets.TcpClient]::new()
 try {
  $connect=$client.ConnectAsync('127.0.0.1',$port)
  if (!$connect.Wait(2000)) { throw 'TCP connect timed out' }
  $stream=$client.GetStream(); $stream.ReadTimeout=2000
  return [char]$stream.ReadByte()
 } finally { $client.Dispose() }
}
function Wait-API([string]$expected) {
 for ($i=0;$i -lt 30;$i++) {
  try { if ((Read-API 18443) -eq $expected) { return } } catch {}
  Start-Sleep -Milliseconds 500
 }
 throw 'API backend did not converge'
}
function Wait-Route([bool]$present) {
 for ($i=0;$i -lt 40;$i++) {
  $routes=@(Get-NetRoute -InterfaceAlias $n -DestinationPrefix '10.254.253.2/32' -ErrorAction SilentlyContinue)
  if (($routes.Count -gt 0) -eq $present) { return }
  Start-Sleep -Milliseconds 500
 }
 throw 'Expected route state was not observed'
}
function Set-Update([string]$json) {
 [IO.File]::WriteAllText("$r\updates.tmp",$json,[Text.UTF8Encoding]::new($false))
 if (Test-Path "$r\updates.json") { [IO.File]::Replace("$r\updates.tmp","$r\updates.json","$r\updates.bak"); Remove-Item "$r\updates.bak" }
 else { [IO.File]::Move("$r\updates.tmp","$r\updates.json") }
}
function Set-Request([string]$payload) {
 $id=[Guid]::NewGuid().ToString('N')
 $encoded=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($payload))
 $json=@{id=$id;secretUID='diagnostic-uid';value=$encoded} | ConvertTo-Json -Compress
 [IO.File]::WriteAllText("$r\request.tmp",$json,[Text.UTF8Encoding]::new($false))
 if (Test-Path "$r\request.json") { [IO.File]::Replace("$r\request.tmp","$r\request.json","$r\request.bak"); Remove-Item "$r\request.bak" }
 else { [IO.File]::Move("$r\request.tmp","$r\request.json") }
 return $id
}
function Wait-Receipt([string]$id,[string]$payload) {
 $sha=[Security.Cryptography.SHA256]::Create()
 try { $expected=([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($payload)))).Replace('-','').ToLowerInvariant() } finally { $sha.Dispose() }
 for ($i=0;$i -lt 40;$i++) {
  try {
   $receipt=Get-Content -Raw "$r\receipt.json" | ConvertFrom-Json
   if ($receipt.id -eq $id -and $receipt.secretUID -eq 'diagnostic-uid' -and $receipt.hash -eq $expected) { return }
  } catch {}
  Start-Sleep -Milliseconds 500
 }
 throw 'Exact request receipt not observed'
}
try {
 if ([Environment]::OSVersion.Version.Build -ne $expectedBuild) { throw 'Unexpected Windows build' }
 if (!(Get-Acl 'C:\ProgramData\CloudProvisioning').AreAccessRulesProtected) { throw 'Unprotected image directory' }
 if ((Get-NetAdapter -Physical | Measure-Object).Count -ne 1) { throw 'Expected one physical NIC' }
 if ((Get-AuthenticodeSignature "$r\wireguard.dll").Status -ne 'Valid') { throw 'Driver signature invalid' }
 $args='"'+$r+'\windows-tunnel.exe" --service-name '+$n+' --interface '+$n+' --peers-file "'+$r+'\peers.json" --updates-file "'+$r+'\updates.json" --cache-file "'+$r+'\cache.json" --request-file "'+$r+'\request.json" --receipt-file "'+$r+'\receipt.json" --poll-interval 1s'
 New-Service -Name $n -BinaryPathName $args -StartupType Manual | Out-Null
 $registered=$true
 New-Item -ItemType Directory -Force "$r\coverage" | Out-Null
 New-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$n" -Name Environment -PropertyType MultiString -Value @("GOCOVERDIR=$r\coverage") -Force | Out-Null
 Start-Service $n
 (Get-Service $n).WaitForStatus('Running',[TimeSpan]::FromSeconds(40))
 Wait-Route $true
 if (@(Get-NetRoute -InterfaceAlias $n -DestinationPrefix '10.244.99.0/24' -ErrorAction SilentlyContinue).Count -ne 0) { throw 'Allowed subnet incorrectly installed as route' }
 foreach ($port in @(18441,18442,18443)) {
  if (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue) { throw 'Diagnostic TCP port already occupied' }
 }
 foreach ($entry in @(@{port=18441;tag='A'},@{port=18442;tag='B'})) {
  $jobs+=Start-Job -ArgumentList $entry.port,$entry.tag -ScriptBlock {
   param($port,$tag)
   $listener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,$port)
   $listener.Start()
   try {
    while ($true) {
     if (!$listener.Pending()) { Start-Sleep -Milliseconds 100; continue }
     $client=$listener.AcceptTcpClient()
     try { $bytes=[Text.Encoding]::ASCII.GetBytes($tag);$client.GetStream().Write($bytes,0,$bytes.Length) } finally { $client.Dispose() }
    }
   } finally { $listener.Stop() }
  }
 }
 foreach ($port in @(18441,18442)) {
  $ready=$false
  for ($i=0;$i -lt 30;$i++) {
   try { $answer=Read-API $port; if ($answer -eq 'A' -or $answer -eq 'B') { $ready=$true; break } } catch {}
   Start-Sleep -Milliseconds 500
  }
  if (!$ready) { throw 'Diagnostic backend did not start' }
 }
 $boot=Get-Content -Raw "$r\peers.json" | ConvertFrom-Json
 $boot | Add-Member -MemberType NoteProperty -Name apiServers -Value @('127.0.0.1:18441','127.0.0.1:18442')
 [IO.File]::WriteAllText("$r\peers.json",($boot | ConvertTo-Json -Depth 8),[Text.UTF8Encoding]::new($false))
 $proxyArgs=$args.Replace('--service-name '+$n,'--service-name '+$proxyName)+' --api-proxy-only --api-proxy-port 18443'
 New-Service -Name $proxyName -BinaryPathName $proxyArgs -StartupType Manual | Out-Null
 $proxyRegistered=$true
 New-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$proxyName" -Name Environment -PropertyType MultiString -Value @("GOCOVERDIR=$r\coverage") -Force | Out-Null
 Start-Service $proxyName
 (Get-Service $proxyName).WaitForStatus('Running',[TimeSpan]::FromSeconds(40))
 Wait-API 'A'
 $identityHash=(Get-FileHash "$r\peers.json" -Algorithm SHA256).Hash
 Set-Update '{"peers":[]}'
 Wait-Route $false
 Stop-Service $n
 (Get-Service $n).WaitForStatus('Stopped',[TimeSpan]::FromSeconds(40))
 if ((Get-Service $proxyName).Status -ne 'Running') { throw 'Tunnel stop affected API service' }
 Wait-API 'A'
 Remove-Item "$r\updates.json"
 Start-Service $n
 (Get-Service $n).WaitForStatus('Running',[TimeSpan]::FromSeconds(40))
 Wait-Route $false
 $cached=Get-Content -Raw "$r\cache.json" | ConvertFrom-Json
 if (@($cached.peers).Count -ne 0) { throw 'Removed peers restored after restart' }
 $bootstrap=Get-Content -Raw "$r\peers.json" | ConvertFrom-Json
 Set-Update (@{peers=@($bootstrap.peers)} | ConvertTo-Json -Depth 8 -Compress)
 Wait-Route $true
 Set-Update (@{peers=@($bootstrap.peers);apiServers=@('127.0.0.1:18442')} | ConvertTo-Json -Depth 8 -Compress)
 Wait-API 'B'
 Set-Update (@{peers=@($bootstrap.peers);apiServers=@('127.0.0.1:18441','127.0.0.1:18442')} | ConvertTo-Json -Depth 8 -Compress)
 Wait-API 'A'
 Stop-Job $jobs[0]
 Wait-API 'B'
 Set-Update '{"peers":[],"privateKey":"replacement"}'
 Start-Sleep -Seconds 3
 Wait-Route $true
 $payload="{`n  `"peers`": []`n}"
 $deliveryID=Set-Request $payload
 Wait-Receipt $deliveryID $payload
 Wait-Route $false
 $payload=@{peers=@($bootstrap.peers)} | ConvertTo-Json -Depth 8 -Compress
 $deliveryID=Set-Request $payload
 Wait-Receipt $deliveryID $payload
 Wait-Route $true
 $badID=Set-Request '{"peers":[{"publicKey":"bad"}]}'
 Start-Sleep -Seconds 3
 if (Test-Path "$r\receipt.json") { throw 'Invalid native request retained an acknowledgment' }
 Wait-Route $true
 if ((Get-FileHash "$r\peers.json" -Algorithm SHA256).Hash -ne $identityHash) { throw 'Bootstrap identity changed' }
 Stop-Service $n
 (Get-Service $n).WaitForStatus('Stopped',[TimeSpan]::FromSeconds(40))
 if (Test-Path "$r\receipt.json") { throw 'Host stop retained receipt' }
 if (@(Get-NetAdapter -Name $n -ErrorAction SilentlyContinue).Count -ne 0) { throw 'Owned adapter leaked after stop' }
 @{windowsBuild=$expectedBuild;singlePhysicalNIC=$true;serviceStartStop=$true;removedPeerRoute=$true;restartRetainedRemoval=$true;readdedPeerRoute=$true;invalidUpdateRejected=$true;identityUnchanged=$true;adapterRemovedOnStop=$true;apiServiceIndependent=$true;apiBackendUpdate=$true;apiBackendFailover=$true;exactDeliveryReceipt=$true;invalidNativeRequestNotAcknowledged=$true;receiptRemovedOnStop=$true} | ConvertTo-Json -Compress
} finally {
 if ($proxyRegistered) {
  Stop-Service $proxyName -ErrorAction SilentlyContinue
  & sc.exe delete $proxyName | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'Diagnostic API service cleanup failed' }
 }
 foreach ($job in $jobs) { Stop-Job $job; Remove-Job $job }
 if ($registered) {
  Stop-Service $n -ErrorAction SilentlyContinue
  & sc.exe delete $n | Out-Null
  if ($LASTEXITCODE -ne 0) { throw 'Diagnostic service cleanup failed' }
 }
}
'''.replace('ROOT',root).replace('MESH',mesh).replace('BUILD','20348' if year=='2022' else '26100')
 archive=a.output/(year+'.zip')
 with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
  z.write(a.service,'windows-tunnel.exe');z.write(a.driver,'wireguard.dll');z.write(a.driver_license,'wireguard-nt-LICENSE.txt');z.writestr('peers.json',json.dumps(doc));z.writestr('run.ps1',script)
 node(year,'-put',root+'.zip',input=archive.read_bytes())
 output=node(year,'--','powershell.exe','-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-Command',f"Expand-Archive -Force '{root}.zip' '{root}'; & '{root}\\run.ps1'")
 (a.output/(year+'-output.log')).write_bytes(output)
 result=json.loads(output.decode('utf-8-sig').strip())
 for k in ['serviceStartStop','removedPeerRoute','restartRetainedRemoval','readdedPeerRoute','invalidUpdateRejected','identityUnchanged','adapterRemovedOnStop','apiServiceIndependent','apiBackendUpdate','apiBackendFailover','exactDeliveryReceipt','invalidNativeRequestNotAcknowledged','receiptRemovedOnStop']:
  if result.get(k) is not True:raise RuntimeError(year+' incomplete evidence')
 # Logs contain no peer document. Preserve service diagnostics before cleanup.
 log=node(year,'--','powershell.exe','-NoProfile','-Command',f"Get-Content -Raw '{root}\\tunnel-service.log'")
 (a.output/(year+'-service.log')).write_bytes(log)
 api_log=node(year,'--','powershell.exe','-NoProfile','-Command',f"Get-Content -Raw '{root}\\api-proxy-service.log'")
 (a.output/(year+'-api-service.log')).write_bytes(api_log)
 if a.coverage:
  raw=node(year,'--','powershell.exe','-NoProfile','-Command',f"@((Get-ChildItem '{root}\\coverage' -File) | ForEach-Object {{ @{{name=$_.Name;data=[Convert]::ToBase64String([IO.File]::ReadAllBytes($_.FullName))}} }}) | ConvertTo-Json -Compress")
  files=json.loads(raw.decode('utf-8-sig'))
  if not isinstance(files,list) or len(files)<2:raise RuntimeError('runtime coverage missing')
  directory=a.output/(year+'-coverage');directory.mkdir()
  for f in files:
   if pathlib.Path(f['name']).name!=f['name'] or not f['name'].startswith('cov'):raise RuntimeError('invalid coverage filename')
   (directory/f['name']).write_bytes(base64.b64decode(f['data'],validate=True))
  result['runtimeCoverageCollected']=True
 node(year,'--','powershell.exe','-NoProfile','-Command',f"$ErrorActionPreference='Stop'; if (Get-Service '{mesh}','{mesh}-api' -ErrorAction SilentlyContinue) {{ throw 'Service still exists' }}; Remove-Item -Recurse -Force '{root}'; Remove-Item -Force '{root}.zip'")
 result['serviceAndGuestArtifactsRemoved']=True
 return year,result

results={};failures={}
with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
 futures={pool.submit(run,y):y for y in ids}
 for f in concurrent.futures.as_completed(futures):
  try:y,result=f.result();results[y]=result
  except Exception as error:failures[futures[f]]=str(error)
report={'scope':'Windows SCM and native tunnel host lifecycle; no CAPI or CNI claim','serviceSHA256':hashlib.sha256(a.service.read_bytes()).hexdigest(),'results':results,'failures':failures}
(a.output/'results.json').write_text(json.dumps(report,indent=2)+'\n')
if failures:raise RuntimeError('Windows service validation failed; inspect private results.json')
print('Both Windows builds passed native service lifecycle checks')
