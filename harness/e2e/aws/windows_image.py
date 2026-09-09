#!/usr/bin/env python3
"""Prepare and capture private Windows images from the run's owned test builders.

Run with setup AWS credentials sourced by the caller. Guest transport uses the
run's derived harness session. Phases persist progress; capture never starts VMs.
"""
from image_policy import owned_image, retained_linux_arns
import argparse,datetime,hashlib,json,os,pathlib,re,subprocess,sys,zipfile
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))
from images.windows.cache import validate_recipe, verify_preparation
from image_recipe import worker_artifact, same_recipe, gpu_evidence, gpu_base_identity, supersede_cache_revision

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--work-dir',required=True,type=pathlib.Path)
p.add_argument('--awsnode',required=True,type=pathlib.Path)
p.add_argument('--phase',required=True,choices=['prepare','generalize','status','capture','authorize','revise'])
p.add_argument('--variant', choices=['cpu','gpu'], default='cpu')
p.add_argument('--windows-version', choices=['2022','2025'], action='append', help='select builder OS; default both')
p.add_argument('--service',type=pathlib.Path)
p.add_argument('--worker',type=pathlib.Path,help='packaged k0s Windows executable for an unjoined image')
p.add_argument('--worker-sha256')
p.add_argument('--worker-version')
p.add_argument('--cache-recipe',type=pathlib.Path,help='verified recipe for an already unpacked Windows runtime cache')
p.add_argument('--supersede-cache-sha256',help='explicitly replace this unprepared cache intent on a stopped builder; preserve its history')
a=p.parse_args();os.umask(0o077)
years=list(dict.fromkeys(a.windows_version or ['2022','2025']))
if a.cache_recipe and (a.phase not in ['revise','prepare'] or len(years)!=1):
 raise RuntimeError('cache recipe applies to revise/prepare for exactly one Windows version')
if a.supersede_cache_sha256 and (a.phase!='revise' or not a.cache_recipe or len(years)!=1):
 raise RuntimeError('cache supersession applies only to revise with one Windows version and a new cache recipe')
cache=validate_recipe(json.loads(a.cache_recipe.read_text()),years[0]) if a.cache_recipe else {}
builds_key='windowsGpuImageBuilds' if a.variant=='gpu' else 'windowsImageBuilds'
builders_key='windowsGpuBuilders' if a.variant=='gpu' else 'windowsProbeInstances'
history_key='windowsGpuImageBuildHistory' if a.variant=='gpu' else 'windowsImageBuildHistory'
worker=worker_artifact(a.worker,a.worker_sha256,a.worker_version)
if worker and a.phase not in ['revise','prepare']:raise RuntimeError('worker artifact applies only to revise/prepare')
state_path=a.work_dir/'resources.json';s=json.loads(state_path.read_text())
repo=pathlib.Path(__file__).resolve().parents[3]
if s.get('cleanedUp'):raise RuntimeError('run is cleaned up')
def save():state_path.write_text(json.dumps(s,indent=2)+'\n')
def aws(service,operation,**inputs):
 r=subprocess.run(['aws','--region',s['region'],service,operation,'--cli-input-json',json.dumps(inputs),'--output','json'],capture_output=True,text=True)
 if r.returncode:raise RuntimeError(service+'/'+operation+' failed')
 return json.loads(r.stdout) if r.stdout.strip() else {}
if aws('sts','get-caller-identity')['Account']!=s['account']:raise RuntimeError('AWS account mismatch')
if a.phase=='authorize':
 approved=list(dict.fromkeys(s.get('authorizedWindowsAMIs',[])))
 for year in years:
  image_id=s.get(builds_key,{}).get(year,{}).get('imageID')
  if not image_id:raise RuntimeError('capture selected Windows candidates before authorization')
  if image_id not in approved:approved.append(image_id)
 # Revalidate retained candidates too; adding a GPU image must not revoke CPU
 # images still used by replacement MachineTemplates.
 for image_id in approved:
  owned_image(aws,s,image_id,'windows')
 role=aws('iam','get-role',RoleName=s['capaRoleName'])['Role']
 if role['Arn']!=s['capaRoleARN'] or {t['Key']:t['Value'] for t in role.get('Tags',[])}.get('cloud-provisioning-test')!=s['runID']:raise RuntimeError('CAPA role ownership mismatch')
 # Re-render this run's explicit resource policy; retain its existing Linux AMI.
 iam=repo/'docs'/'iam'
 policy=json.loads(subprocess.check_output(['python3',str(iam/'render.py'),str(iam/'capa.json'),str(a.work_dir/'iam-values.json')],text=True))
 launch=next(item for item in policy['Statement'] if item.get('Sid')=='UseApprovedLaunchResources')
 launch['Resource']+=['arn:aws:ec2:'+s['region']+'::image/'+id for id in approved]
 launch['Resource']+=retained_linux_arns(aws,s)
 launch['Resource']=list(dict.fromkeys(launch['Resource']))
 aws('iam','put-role-policy',RoleName=s['capaRoleName'],PolicyName='cldt-test',PolicyDocument=json.dumps(policy))
 (a.work_dir/'capa-policy.json').write_text(json.dumps(policy,indent=2)+'\n')
 s['authorizedWindowsAMIs']=approved
 for year in years:s[builds_key][year]['imageState']='available'
 save()
 print('Authorized selected owned Windows candidates while retaining existing Windows and Linux image permissions')
 raise SystemExit(0)
ids={y:s[builders_key][y]['instanceID'] for y in years}
instances={i['InstanceId']:i for r in aws('ec2','describe-instances',InstanceIds=list(ids.values()))['Reservations'] for i in r['Instances']}
for id in ids.values():
 i=instances[id]
 if i['VpcId']!=s['vpcID'] or {t['Key']:t['Value'] for t in i.get('Tags',[])}.get('cloud-provisioning-test')!=s['runID'] or i.get('Platform')!='windows' or len(i['NetworkInterfaces'])!=1:raise RuntimeError('builder ownership/platform preflight failed')
def artifact_name(year, suffix):
 return year+('-gpu' if a.variant=='gpu' else '')+suffix
def node(year,*args,input=None):
 r=subprocess.run([str(a.awsnode.resolve()),'-work-dir',str(a.work_dir.resolve()),'-instance-id',ids[year],*args],input=input,capture_output=True,timeout=360)
 if r.returncode:
  (a.work_dir/(artifact_name(year,'-image-error.log'))).write_bytes(r.stdout+r.stderr)
  raise RuntimeError(year+' guest operation failed; inspect private image-error.log')
 return r.stdout

for year,id in ids.items():
 record=s.setdefault(builds_key,{}).setdefault(year,{})
 state=instances[id]['State']['Name']
 if a.phase=='revise':
  if not a.service or not a.service.is_file():raise RuntimeError('revision requires --service artifact')
  sha=hashlib.sha256(a.service.read_bytes()).hexdigest()
  if a.variant=='gpu':
   base=s.get('windowsImageBuilds',{}).get(year,{})
   builder=s[builders_key][year]
   gpu_base_identity(builder,base,sha,worker)
  if cache and cache['runtime']['workerSHA256']!=worker.get('workerSHA256',record.get('workerSHA256')):
   raise RuntimeError('cache recipe must match the revised worker')
  if a.variant=='gpu' and not cache:raise RuntimeError('GPU revision requires an explicit changed cache recipe and preserves its CPU base and driver')
  if a.supersede_cache_sha256:
   replacement=supersede_cache_revision(record,a.supersede_cache_sha256,cache,year,sha,worker,state,datetime.datetime.now(datetime.timezone.utc).isoformat())
   image=aws('ec2','describe-images',ImageIds=[record['revisionOf']])['Images'][0]
   if image['State']!='available' or image['Public'] or image['OwnerId']!=s['account'] or {t['Key']:t['Value'] for t in image.get('Tags',[])}.get('cloud-provisioning-test')!=s['runID']:raise RuntimeError('previous capture must remain available, private and owned')
   record=replacement;s[builds_key][year]=record;save()
  if record.get('revisionRequestedAt') and not record.get('preparedAt'):
   if record.get('requestedSHA256')!=sha or record.get('requestedWorker',{})!=worker or record.get('requestedCacheRecipe',{})!=cache:raise RuntimeError('another image revision is already in progress')
  else:
   if state!='stopped' or not record.get('imageID') or (same_recipe(record,sha,worker) and record.get('cacheRecipe',{})==cache):raise RuntimeError('revision requires a stopped captured builder and a changed service, worker or cache recipe')
   image=aws('ec2','describe-images',ImageIds=[record['imageID']])['Images'][0]
   if image['State']!='available' or image['Public'] or image['OwnerId']!=s['account'] or {t['Key']:t['Value'] for t in image.get('Tags',[])}.get('cloud-provisioning-test')!=s['runID']:raise RuntimeError('previous image must be available, private and owned before revision')
   s.setdefault(history_key,{}).setdefault(year,[]).append(dict(record,instanceID=id))
   record={'revisionOf':record['imageID'],'workerSHA256':record.get('workerSHA256'),'requestedSHA256':sha,'requestedWorker':worker,'requestedCacheRecipe':cache,'revisionRequestedAt':datetime.datetime.now(datetime.timezone.utc).isoformat()}
   s[builds_key][year]=record;save()
  if state=='stopped':
   # The old diagnostic userdata must not rerun after EC2Launch's state reset.
   # Existing captured AMIs/snapshots remain immutable and cleanup-owned.
   aws('ec2','modify-instance-attribute',InstanceId=id,UserData={'Value':''})
   aws('ec2','start-instances',InstanceIds=[id])
  elif state not in ['pending','running']:raise RuntimeError('wait for builder stop before resuming revision')
  print(year+' builder revision requested; wait for SSM then prepare, generalize and capture again')
  continue
 if a.phase=='status':
  summary={'windows':year,'instanceState':state,'generalizeRequestedAt':record.get('generalizeRequestedAt'),'imageID':record.get('imageID')}
  if record.get('imageID'):
   image=aws('ec2','describe-images',ImageIds=[record['imageID']])['Images'][0]
   if image['Public'] or image['OwnerId']!=s['account']:raise RuntimeError('candidate image is public or has the wrong owner')
   summary['imageState']=image['State'];summary['public']=image['Public']
   roots=[m.get('Ebs',{}).get('VolumeSize') for m in image.get('BlockDeviceMappings',[]) if m.get('DeviceName')==image.get('RootDeviceName')]
   if len(roots)!=1 or type(roots[0]) is not int or roots[0]<1:raise RuntimeError('candidate image root volume is missing or invalid')
   record['rootVolumeGiB']=roots[0];summary['rootVolumeGiB']=roots[0]
   record['imageState']=image['State']
   snapshots=[m['Ebs']['SnapshotId'] for m in image.get('BlockDeviceMappings',[]) if 'Ebs' in m]
   record['snapshotIDs']=snapshots
   for owned in s.get('retiredImages',[]):
    if owned['id']==record['imageID']:owned['snapshots']=snapshots
   save()
  print(json.dumps(summary));continue
 if a.phase=='prepare':
  if state!='running' or record.get('generalizeRequestedAt'):raise RuntimeError('prepare requires a running, not generalized builder')
  if not a.service or not a.service.is_file():raise RuntimeError('--service artifact required')
  sha=hashlib.sha256(a.service.read_bytes()).hexdigest()
  if a.variant=='gpu':
   base=s.get('windowsImageBuilds',{}).get(year,{})
   builder=s[builders_key][year]
   gpu_base_identity(builder,base,sha,worker)
  if record.get('revisionRequestedAt') and (record.get('requestedSHA256')!=sha or record.get('requestedWorker',{})!=worker or record.get('requestedCacheRecipe',{})!=cache):raise RuntimeError('preparation artifacts differ from requested revision')
  archive=a.work_dir/(artifact_name(year,'-image-stage.zip'));root=r'C:\ProgramData\CloudProvisioning'
  script=r'''$ErrorActionPreference='Stop'
$r='C:\ProgramData\CloudProvisioning'
if ([Environment]::OSVersion.Version.Build -ne BUILD) { throw 'OS build mismatch' }
if (!(Get-Acl $r).AreAccessRulesProtected) { throw 'Unprotected image directory' }
if ((Get-Item $r).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Image directory is a reparse point' }
if ((Get-NetAdapter -Physical | Measure-Object).Count -ne 1) { throw 'Expected one physical NIC' }
if (Get-Service k0sworker,'cloud-provisioning-*','cldt*' -ErrorAction SilentlyContinue) { throw 'Image has a worker or diagnostic service installed' }
__GUARD_VARIANT__
& "$r\image-stage\prepare-tunnel.ps1" -Source "$r\image-stage\windows-tunnel.exe" -SHA256 '__SHA__'
__PREPARE_WORKER__
$verified=& "$r\image-stage\verify-k0s.ps1" | ConvertFrom-Json
Import-Module AWS.Tools.SecretsManager -RequiredVersion 5.0.268 -ErrorAction Stop
if ((Get-AuthenticodeSignature "$r\wireguard.dll").Status -ne 'Valid') { throw 'WireGuardNT signature invalid' }
if (@(Get-NetAdapter | Where-Object Name -Match '^cldt[0-9a-f]{8}$').Count) { throw 'Diagnostic adapter remains' }
__VERIFY_GPU__
__VERIFY_CACHE__
$keep=@('k0s.exe','wireguard.dll','wireguard-nt-LICENSE.txt','image-preparation.json','windows-tunnel.exe','tunnel-image.json','image-verification.json')
__KEEP_GPU__
# This is the run-owned application's diagnostic directory, never a user profile.
# Preserve only image recipe assets; peer documents, archives and scripts go away.
foreach ($entry in Get-ChildItem -LiteralPath $r -Force) {
 if ($entry.Name -notin $keep) {
  if ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Unexpected reparse point in image state' }
  Remove-Item -LiteralPath $entry.FullName -Recurse -Force
 }
}
__RECORD_GPU__
__RECORD_CACHE__
$verified | Add-Member -NotePropertyName tunnelSHA256 -NotePropertyValue '__SHA__'
$verified | Add-Member -NotePropertyName diagnosticStateRemoved -NotePropertyValue $true
$verified | Add-Member -NotePropertyName awsToolsVersion -NotePropertyValue '5.0.268'
$verified | ConvertTo-Json | Set-Content -Encoding UTF8 "$r\image-verification.json"
$verified | ConvertTo-Json -Compress
'''.replace('BUILD','20348' if year=='2022' else '26100').replace('__SHA__',sha)
  worker_command=(r'& "$r\image-stage\prepare-worker.ps1" -Source "$r\image-stage\k0s.exe" -SHA256 '+worker['workerSHA256']+' -Version '+worker['workerVersion']) if worker else ''
  script=script.replace('__PREPARE_WORKER__',worker_command)
  script=script.replace('__GUARD_VARIANT__', "if (Test-Path (Join-Path $r 'nvidia-image.json')) { throw 'Use the GPU image variant to retain driver evidence' }" if a.variant=='cpu' else '')
  script=script.replace('__VERIFY_GPU__', r'$gpu=& "$r\image-stage\verify-nvidia.ps1" | ConvertFrom-Json' if a.variant=='gpu' else '')
  script=script.replace('__KEEP_GPU__', "$keep+=@('nvidia-image.json','nvidia-verification.json')" if a.variant=='gpu' else '')
  script=script.replace('__RECORD_GPU__', '$verified | Add-Member -NotePropertyName gpu -NotePropertyValue $gpu' if a.variant=='gpu' else '')
  script=script.replace('__VERIFY_CACHE__', r'$cache=& "$r\image-stage\verify-cache.ps1" -Recipe "$r\image-stage\cache-recipe.json" | ConvertFrom-Json' if cache else '')
  script=script.replace('__RECORD_CACHE__', '$verified | Add-Member -NotePropertyName cache -NotePropertyValue $cache' if cache else '')
  script=script.replace('ConvertTo-Json', 'ConvertTo-Json -Depth 8')
  with zipfile.ZipFile(archive,'w',zipfile.ZIP_DEFLATED) as z:
   z.write(a.service,'windows-tunnel.exe');z.write(repo/'images/windows/prepare-tunnel.ps1','prepare-tunnel.ps1');z.write(repo/'images/windows/verify-k0s.ps1','verify-k0s.ps1');z.writestr('prepare.ps1',script)
   if a.variant=='gpu':z.write(repo/'images/windows/verify-nvidia.ps1','verify-nvidia.ps1')
   if cache:
    z.writestr('cache-recipe.json',json.dumps(cache));z.write(repo/'images/windows/verify-cache.ps1','verify-cache.ps1');z.write(repo/'images/windows/inspect-cache.ps1','inspect-cache.ps1');z.write(repo/'images/windows/cache-runtime.ps1','cache-runtime.ps1')
   if worker:
    z.write(a.worker,'k0s.exe');z.write(repo/'images/windows/prepare-worker.ps1','prepare-worker.ps1')
  node(year,'-put',root+r'\image-stage.zip',input=archive.read_bytes())
  output=node(year,'--','powershell.exe','-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-Command',rf"Expand-Archive -Force '{root}\image-stage.zip' '{root}\image-stage'; & '{root}\image-stage\prepare.ps1'")
  evidence=json.loads(output.decode('utf-8-sig').strip())
  if a.variant=='gpu':
   record['gpu']=gpu_evidence(evidence.get('gpu'),year)
   if evidence.get('k0sSHA256')!=base.get('workerSHA256') or evidence.get('k0s')!=base.get('workerVersion'):
    raise RuntimeError('GPU builder worker differs from the recorded CPU base')
  if cache:
   if evidence.get('k0sSHA256')!=cache['runtime']['workerSHA256']:raise RuntimeError('Cache recipe worker differs from prepared worker')
   record['cacheVerification']=verify_preparation(evidence.get('cache',{}),cache,year)
   record['cacheRecipe']=cache
  elif record.get('cacheRecipe'):raise RuntimeError('Explicitly supply the existing cache recipe')
  if evidence.get('diagnosticStateRemoved') is not True or evidence.get('tunnelSHA256')!=sha:raise RuntimeError('incomplete image evidence')
  if worker and (evidence.get('k0sSHA256')!=worker['workerSHA256'] or evidence.get('k0s')!=worker['workerVersion']):raise RuntimeError('worker image verification mismatch')
  (a.work_dir/(artifact_name(year,'-image-preparation.json'))).write_text(json.dumps(evidence,indent=2)+'\n')
  record.update(preparedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(),tunnelSHA256=sha,workerSHA256=evidence['k0sSHA256'],workerVersion=evidence['k0s']);save();print(year+' image assets verified and diagnostic state removed')
 elif a.phase=='generalize':
  if a.variant=='gpu':
   gpu_evidence(record.get('gpu'),year)
   gpu_base_identity(s[builders_key][year],s.get('windowsImageBuilds',{}).get(year,{}),record.get('tunnelSHA256'))
  if record.get('requestedCacheRecipe') and (record.get('cacheRecipe')!=record['requestedCacheRecipe'] or not record.get('cacheVerification',{}).get('passed')):raise RuntimeError('Cache revision must pass native preparation before generalization')
  if not record.get('preparedAt') or state!='running':raise RuntimeError('generalize requires a prepared running builder')
  if record.get('generalizeRequestedAt'):raise RuntimeError('generalization already requested; inspect status rather than restarting')
  # A self-removing scheduled task outlives the SSM command and gives transport
  # time to return. EC2Launch owns agent reset, Sysprep and the final shutdown.
  script=r'''$ErrorActionPreference='Stop'
$exe='C:\Program Files\Amazon\EC2Launch\EC2Launch.exe'
if (!(Test-Path $exe)) { throw 'EC2Launch v2 is missing' }
if (!(Test-Path 'C:\ProgramData\CloudProvisioning\image-verification.json')) { throw 'Image verification missing' }
if(Get-Service k0sworker,cldt-image-cache -ErrorAction SilentlyContinue) { throw 'Worker or cache service remains installed' }
if(Get-Process containerd -ErrorAction SilentlyContinue) { throw 'Runtime must be stopped before Sysprep' }
$task='CloudProvisioning-Generalize'
$body="Start-Sleep -Seconds 15; Unregister-ScheduledTask -TaskName '$task' -Confirm:`$false; & '$exe' sysprep --clean --shutdown"
$encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($body))
$action=New-ScheduledTaskAction -Execute 'powershell.exe' -Argument ('-NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand '+$encoded)
Register-ScheduledTask -TaskName $task -Action $action -User 'SYSTEM' -RunLevel Highest | Out-Null
Start-ScheduledTask -TaskName $task
'Generalization scheduled'
'''
  # Persist intent before dispatch so an interrupted transport cannot cause a
  # duplicate generalization on the next run.
  record['generalizeRequestedAt']=datetime.datetime.now(datetime.timezone.utc).isoformat();save()
  node(year,'--','powershell.exe','-NoProfile','-NonInteractive','-Command',script)
  print(year+' EC2Launch Sysprep shutdown scheduled')
 elif a.phase=='capture':
  if record.get('requestedCacheRecipe') and (record.get('cacheRecipe')!=record['requestedCacheRecipe'] or not record.get('cacheVerification',{}).get('passed')):raise RuntimeError('Cache revision must pass native preparation before capture')
  if a.variant=='gpu':
   gpu_evidence(record.get('gpu'),year)
   gpu_base_identity(s[builders_key][year],s.get('windowsImageBuilds',{}).get(year,{}),record.get('tunnelSHA256'))
  if not record.get('generalizeRequestedAt') or state!='stopped':raise RuntimeError('capture requires a builder stopped after generalization dispatch')
  if record.get('imageID'):print(year+' image already recorded: '+record['imageID']);continue
  tags=[{'Key':'cloud-provisioning-test','Value':s['runID']},{'Key':'windows-build','Value':year}]
  worker_sha=record.get('workerSHA256','')
  if not re.fullmatch(r'[a-f0-9]{64}',worker_sha):raise RuntimeError('capture requires the verified worker artifact identity')
  name=s['runID']+'-'+year+'-'+record['tunnelSHA256'][:12]+'-k0s-'+worker_sha[:12]
  if a.variant=='gpu':
   name+='-gpu-'+record['gpu']['packageSHA256'][:12]
   tags.append({'Key':'cloud-provisioning-image-variant','Value':'gpu'})
  if record.get('cacheRecipe'):
   validate_recipe(record['cacheRecipe'],year)
   if record['cacheRecipe']['runtime']['workerSHA256']!=worker_sha:raise RuntimeError('Cache capture worker identity changed')
   if not record.get('cacheVerification',{}).get('passed'):raise RuntimeError('Cache capture requires native preparation evidence')
   if record['cacheVerification'].get('checks',{}).get('dataDirectoryInheritsPermissions') is not True:raise RuntimeError('Cache capture requires native data-directory permission evidence')
   name+='-cache-'+record['cacheRecipe']['sha256'][:12]
  result=aws('ec2','create-image',InstanceId=id,Name=name,Description='Private test candidate: k0s 1.36.2 Windows native tunnel and secure CAPA consumer',NoReboot=True,TagSpecifications=[{'ResourceType':'image','Tags':tags},{'ResourceType':'snapshot','Tags':tags}])
  record['imageID']=result['ImageId'];record['imageState']='pending'
  s.setdefault('retiredImages',[]).append({'id':result['ImageId'],'snapshots':[]})
  save();print(year+' private test candidate capture requested: '+result['ImageId'])
