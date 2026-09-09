#!/usr/bin/env python3
"""Exercise the native tunnel backend on the run's two single-NIC Windows VMs.

Requires the AWS node tool, Windows probe binary, signed WireGuardNT DLL and
license. Uses the run's derived session; does not provision or join nodes.
"""
import argparse, base64, concurrent.futures, hashlib, json, os, pathlib, subprocess, time, zipfile
from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PrivateFormat, PublicFormat, NoEncryption

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--work-dir',required=True,type=pathlib.Path)
p.add_argument('--awsnode',required=True,type=pathlib.Path)
p.add_argument('--probe',required=True,type=pathlib.Path)
p.add_argument('--driver',required=True,type=pathlib.Path)
p.add_argument('--driver-license',required=True,type=pathlib.Path)
p.add_argument('--family',choices=['ipv4','ipv6'],required=True)
p.add_argument('--output',required=True,type=pathlib.Path)
a=p.parse_args();os.umask(0o077)
s=json.loads((a.work_dir/'resources.json').read_text())
if s.get('cleanedUp'):raise RuntimeError('run is cleaned up')
for f in [a.awsnode,a.probe,a.driver,a.driver_license]:
 if not f.is_file():raise RuntimeError('required build artifact missing')
if hashlib.sha256(a.driver.read_bytes()).hexdigest()!='b1b85e072c45d81358be29d94c599dc76652f912be8c0f0a41e2d5d89a6461d3':raise RuntimeError('unrecognized WireGuardNT DLL')
creds=json.loads((a.work_dir/'harness-session.json').read_text())['Credentials']
env=dict(os.environ, AWS_ACCESS_KEY_ID=creds['AccessKeyId'],AWS_SECRET_ACCESS_KEY=creds['SecretAccessKey'],AWS_SESSION_TOKEN=creds['SessionToken'])
ids={y:s['windowsProbeInstances'][y]['instanceID'] for y in ['2022','2025']}
raw=subprocess.check_output(['aws','--region',s['region'],'ec2','describe-instances','--instance-ids',*ids.values(),'--output','json'],env=env)
instances={i['InstanceId']:i for r in json.loads(raw)['Reservations'] for i in r['Instances']}
for id in ids.values():
 i=instances[id]
 if i['VpcId']!=s['vpcID'] or {t['Key']:t['Value'] for t in i['Tags']}.get('cloud-provisioning-test')!=s['runID'] or len(i['NetworkInterfaces'])!=1 or i.get('Platform')!='windows' or i['State']['Name']!='running':raise RuntimeError('instance ownership, platform or single-NIC preflight failed')
a.output.mkdir(parents=True,exist_ok=False)
mesh='cldt'+hashlib.sha256((s['runID']+a.family+str(a.output.resolve())).encode()).hexdigest()[:8]
keys={y:X25519PrivateKey.generate() for y in ids}
hosts={y:(f'10.254.254.{n}' if a.family=='ipv4' else f'fd00:c1d7::{n}') for n,y in enumerate(ids,1)}
bits='32' if a.family=='ipv4' else '128'
root='C:\\ProgramData\\CloudProvisioning\\'+mesh

def node(year,*args,input=None):
 r=subprocess.run([str(a.awsnode.resolve()),'-work-dir',str(a.work_dir.resolve()),'-instance-id',ids[year],*args],input=input,capture_output=True,timeout=360)
 if r.returncode:raise RuntimeError(year+' guest command failed: '+r.stderr.decode(errors='replace'))
 return r.stdout

def run(year):
 other=next(y for y in ids if y!=year)
 doc={'privateKey':base64.b64encode(keys[year].private_bytes(Encoding.Raw,PrivateFormat.Raw,NoEncryption())).decode(),'localAddress':hosts[year]+'/'+bits,'peers':[{'publicKey':base64.b64encode(keys[other].public_key().public_bytes(Encoding.Raw,PublicFormat.Raw)).decode(),'endpoint':instances[ids[other]]['PublicIpAddress']+':51820','allowedIPs':[hosts[other]+'/'+bits, '10.244.99.0/24' if a.family=='ipv4' else 'fd00:244:99::/64'],'routeHosts':[hosts[other]+'/'+bits]}]}
 archive=a.output/(year+'.zip')
 # The protected root is an image prerequisite; no installers run here.
 script=f'''$ErrorActionPreference='Stop'
$r='{root}'
if (!(Get-Acl 'C:\\ProgramData\\CloudProvisioning').AreAccessRulesProtected) {{ throw 'Unprotected image directory' }}
if ((Get-NetAdapter -Physical | Measure-Object).Count -ne 1) {{ throw 'Expected one physical NIC' }}
if ((Get-AuthenticodeSignature "$r\\wireguard.dll").Status -ne 'Valid') {{ throw 'Driver signature invalid' }}
New-NetFirewallRule -Name '{mesh}-udp' -DisplayName '{mesh}-udp' -Direction Inbound -Action Allow -Protocol UDP -LocalPort 51820 -RemoteAddress '{instances[ids[other]]['PublicIpAddress']}' | Out-Null
New-NetFirewallRule -Name '{mesh}-tcp' -DisplayName '{mesh}-tcp' -Direction Inbound -Action Allow -Protocol TCP -LocalPort 18080 -RemoteAddress '{hosts[other]}' | Out-Null
$env:GOCOVERDIR="$r\\coverage"
New-Item -ItemType Directory -Force $env:GOCOVERDIR | Out-Null
try {{
 $process = Start-Process -Wait -PassThru -FilePath "$r\\tunnelprobe.exe" -ArgumentList @('-peers-file',"$r\\peers.json",'-target','{hosts[other]}','-interface','{mesh}','-report',"$r\\result.json") -RedirectStandardOutput "$r\\stdout.log" -RedirectStandardError "$r\\stderr.log"
 if ($process.ExitCode -ne 0) {{ throw 'Native tunnel probe failed' }}
 Get-Content -Raw "$r\\result.json"
}} finally {{
 Get-NetFirewallRule -Name '{mesh}-udp','{mesh}-tcp' -ErrorAction SilentlyContinue | Remove-NetFirewallRule
}}
'''
 with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
  z.write(a.probe,'tunnelprobe.exe');z.write(a.driver,'wireguard.dll');z.write(a.driver_license,'wireguard-nt-LICENSE.txt');z.writestr('peers.json',json.dumps(doc));z.writestr('run.ps1',script)
 node(year,'-put',root+'.zip',input=archive.read_bytes())
 output=node(year,'--','powershell.exe','-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-Command',f"Expand-Archive -Force '{root}.zip' '{root}'; & '{root}\\run.ps1'")
 (a.output/(year+'-output.log')).write_bytes(output)
 result=json.loads(output.decode('utf-8-sig').strip())
 if result.get('initialConnections')!=5 or not all(result.get(k) for k in ['removedPeerBlocked','readdedPeerConnected','kernelRouteRemovalAndReaddVerified']):raise RuntimeError(year+' evidence did not pass')
 (a.output/(year+'-result.json')).write_text(json.dumps(result,indent=2)+'\n')
 coverage=node(year,'--','powershell.exe','-NoProfile','-NonInteractive','-Command',"$files=@(Get-ChildItem '"+root+"\\coverage' -File | ForEach-Object { @{name=$_.Name;data=[Convert]::ToBase64String([IO.File]::ReadAllBytes($_.FullName))} }); ConvertTo-Json -InputObject $files -Compress")
 files=json.loads(coverage)
 if len(files)<2:raise RuntimeError('build the probe with Go coverage enabled')
 directory=a.output/('coverage-'+year);directory.mkdir()
 for f in files:
  if pathlib.Path(f['name']).name!=f['name'] or not f['name'].startswith('cov'):raise RuntimeError('invalid coverage filename')
  (directory/f['name']).write_bytes(base64.b64decode(f['data'],validate=True))
 return year,result

# Each VM must start its responder while the other is attempting connections.
results={};failures={}
with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
 futures={pool.submit(run,y):y for y in ids}
 for f in concurrent.futures.as_completed(futures):
  try:y,result=f.result();results[y]=result
  except Exception as error:failures[futures[f]]=str(error)
report={'family':a.family,'scope':'native Windows WireGuard backend; no CAPI or CNI claim','singleNICPreflight':True,'probeSHA256':hashlib.sha256(a.probe.read_bytes()).hexdigest(),'driverSHA256':hashlib.sha256(a.driver.read_bytes()).hexdigest(),'results':results,'failures':failures}
(a.output/'results.json').write_text(json.dumps(report,indent=2)+'\n')
if failures:raise RuntimeError('Windows tunnel validation failed; inspect private results.json')
print('Both Windows builds passed '+a.family+' native tunnel lifecycle checks')
