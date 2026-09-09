#!/usr/bin/env python3
"""Observe a real CAPA Windows worker through Kubernetes, EC2 and native SSM.

No status is synthesized and no guest software is installed. Output is a join
checkpoint, not proof of the add/remove, traffic or NetworkPolicy matrix.
"""
import argparse
import base64
import datetime
import hashlib
import json
import os
import pathlib
import re
import subprocess

GUEST = r'''$ErrorActionPreference='Stop'
$r='C:\ProgramData\CloudProvisioning'
$k=Join-Path $r 'k0s.exe'
$containerd='C:\var\lib\k0s\bin\containerd.exe'
$runtime=$null
if (Test-Path $containerd) { $runtime=(& $containerd --version | Out-String).Trim() }
$services=@(Get-Service k0sworker,'cloud-provisioning-*' -ErrorAction SilentlyContinue | ForEach-Object { @{name=$_.Name;status=$_.Status.ToString()} })
$runningTunnels=@(Get-CimInstance Win32_Service | Where-Object { $_.Name -match '^cloud-provisioning-cldt[0-9a-f]{8}$' -and $_.State -eq 'Running' } | ForEach-Object { $process=Get-Process -Id $_.ProcessId; @{name=$_.Name;processId=$_.ProcessId;executableSHA256=(Get-FileHash $process.Path).Hash.ToLowerInvariant()} })
$meshes=@(Get-ChildItem $r -Directory | Where-Object Name -Match '^cldt[0-9a-f]{8}$' | ForEach-Object {
 $marker=Get-Item (Join-Path $_.FullName 'bootstrap-installed') -ErrorAction SilentlyContinue
 $request=$null;$receipt=$null
 if(Test-Path (Join-Path $_.FullName 'request.json')) { $request=Get-Content (Join-Path $_.FullName 'request.json') -Raw | ConvertFrom-Json }
 if(Test-Path (Join-Path $_.FullName 'receipt.json')) { $receipt=Get-Content (Join-Path $_.FullName 'receipt.json') -Raw | ConvertFrom-Json }
 @{name=$_.Name;installed=($null -ne $marker);installedAt=$(if ($marker) { $marker.LastWriteTimeUtc.ToString('o') } else { $null });receiptPresent=($null -ne $receipt);requestID=$request.id;requestSecretUID=$request.secretUID;receipt=$receipt}
})
@{build=[Environment]::OSVersion.Version.Build;bootTime=(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o');k0sVersion=((& $k version | Out-String).Trim());k0sSHA256=(Get-FileHash $k).Hash.ToLowerInvariant();tunnelSHA256=(Get-FileHash (Join-Path $r 'windows-tunnel.exe')).Hash.ToLowerInvariant();containerdVersion=$runtime;
 physicalNICs=@(Get-NetAdapter -Physical | ForEach-Object { @{name=$_.Name;status=$_.Status.ToString()} });
 services=$services;runningTunnels=$runningTunnels;meshes=$meshes} | ConvertTo-Json -Depth 8 -Compress
'''

def associated(machine, node):
    # CAPI v1beta2 nodeRef carries a name, not an ObjectReference UID. Record
    # the actual Node UID separately so replacement tests can compare it.
    return (machine.get('status',{}).get('phase')=='Running'
            and machine.get('status',{}).get('nodeRef',{}).get('name')==node['metadata']['name']
            and bool(node['metadata'].get('uid'))
            and bool(machine['spec'].get('providerID'))
            and node['spec'].get('providerID')==machine['spec']['providerID'])

def image_artifacts(image, guest, expected_running=None):
    expected_running = expected_running or image.get('tunnelSHA256')
    running = guest.get('runningTunnels')
    running_matches = (bool(expected_running) and isinstance(running, list) and bool(running)
                       and all(isinstance(item, dict) and isinstance(item.get('name'), str) and
                               re.fullmatch(r'cloud-provisioning-cldt[0-9a-f]{8}', item.get('name', '')) and
                               type(item.get('processId')) is int and item['processId'] > 0 and
                               item.get('executableSHA256') == expected_running for item in running))
    return {
        'expectedWorkerVersion':bool(image.get('workerVersion')) and guest.get('k0sVersion')==image['workerVersion'],
        'expectedWorkerHash':bool(image.get('workerSHA256')) and guest.get('k0sSHA256')==image['workerSHA256'],
        'expectedTunnelHash':bool(image.get('tunnelSHA256')) and guest.get('tunnelSHA256')==image['tunnelSHA256'],
        'expectedRunningTunnelHash':bool(running_matches),
    }

def delivered(secret, mesh, now):
    """Require the live service's exact, fresh receipt and publisher ack."""
    receipt=mesh.get('receipt') or {}
    uid=secret['metadata']['uid']
    digest=hashlib.sha256(base64.b64decode(secret['data']['peers.json'])).hexdigest()
    try:
        age=(now-datetime.datetime.fromisoformat(receipt['appliedAt'].replace('Z','+00:00'))).total_seconds()
    except (KeyError, ValueError, TypeError):
        return False
    return (bool(mesh.get('requestID')) and receipt.get('id')==mesh['requestID']
            and mesh.get('requestSecretUID')==uid and receipt.get('secretUID')==uid
            and receipt.get('hash')==digest and 0<=age<=30
            and secret['metadata'].get('annotations',{}).get('cloud-provisioning.appmana.com/applied')==digest)

def gateway_ready(configmaps, machine_uid, node_uid):
    """Require current UID-bound gateway leases before gateway-mode workload tests."""
    if not machine_uid or not node_uid:
        return False
    active=[]
    try:
        for cm in configmaps['items']:
            if not cm['metadata']['name'].startswith('network-attachment-'):
                continue
            record=json.loads(cm['data']['record.json'])
            worker=record['plan']['worker']
            if worker['uid']==machine_uid and record['phase']!='Complete':
                active.append(not cm['metadata'].get('deletionTimestamp') and
                              record['phase']=='Ready' and worker.get('nodeUID')==node_uid)
    except (KeyError, TypeError, ValueError):
        return False
    return bool(active) and all(active)


def record_guest_failure(report, output, result):
    """Retain executor stderr privately instead of replacing it with a generic failure."""
    output.parent.mkdir(parents=True, exist_ok=True)
    diagnostic = output.with_suffix('.guest.stderr')
    with open(diagnostic, 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as stream:
        stream.write(result.stderr)
    report['guestObservation'] = {'exitCode': result.returncode,
                                'stderrArtifact': diagnostic.name,
                                'status': 'failed; inspect original executor'}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir',required=True,type=pathlib.Path)
    p.add_argument('--awsnode',required=True,type=pathlib.Path)
    p.add_argument('--api-server',required=True)
    p.add_argument('--bastion',default='clab-cldt-bastion')
    p.add_argument('--name',required=True)
    p.add_argument('--windows-version',required=True,choices=['2022','2025'])
    p.add_argument('--variant',choices=['cpu','gpu'],default='cpu')
    p.add_argument('--output',required=True,type=pathlib.Path)
    p.add_argument('--require-bootstrap-completion',action='store_true',help='Require an identity-bound terminal receipt from the detached CAPA secure consumer')
    p.add_argument('--require-gateway',action='store_true',help='Require Ready gateway attachments bound to this Machine and Node before workload tests')
    p.add_argument('--require-peer-delivery',action='store_true',help='Also require fresh native receipts and exact publisher acknowledgement')
    p.add_argument('--expected-running-tunnel-sha256', help='Explicit live canary hash; baked artifact checks remain unchanged')
    a=p.parse_args();os.umask(0o077)
    if a.expected_running_tunnel_sha256 and not re.fullmatch('[a-f0-9]{64}',a.expected_running_tunnel_sha256):p.error('require a lowercase SHA256 for the running canary')
    s=json.loads((a.work_dir/'resources.json').read_text())
    if s.get('cleanedUp'):raise RuntimeError('run is cleaned up')
    if a.output.exists():raise RuntimeError('use a new evidence path')
    a.output.parent.mkdir(parents=True,exist_ok=True)
    creds=json.loads((a.work_dir/'harness-session.json').read_text())['Credentials']
    env=dict(os.environ,AWS_ACCESS_KEY_ID=creds['AccessKeyId'],AWS_SECRET_ACCESS_KEY=creds['SecretAccessKey'],AWS_SESSION_TOKEN=creds['SessionToken'])
    def aws(operation,inputs):
        result=subprocess.run(['aws','--region',s['region'],'ec2',operation,'--cli-input-json',json.dumps(inputs),'--output','json'],capture_output=True,text=True,env=env)
        if result.returncode:raise RuntimeError('EC2 observation failed')
        return json.loads(result.stdout)
    def kube(*args):
        result=subprocess.run(['docker','exec',a.bastion,'kubectl','--server='+a.api_server,*args,'-o','json'],capture_output=True,text=True)
        if result.returncode:raise RuntimeError('Kubernetes observation failed')
        return json.loads(result.stdout)
    site=kube('get','nodes','-l','node-role.kubernetes.io/control-plane')
    site_versions=sorted({n['status']['nodeInfo']['kubeletVersion'] for n in site['items']})
    machine=kube('-n','cloud-provisioning','get','machine',a.name)
    if machine['spec']['clusterName']!=s['runID']:raise RuntimeError('Machine belongs to another cluster')
    provider=machine['spec'].get('providerID','')
    if not provider.startswith('aws:///'):raise RuntimeError('CAPI has not published an AWS provider ID')
    instance_id=provider.rsplit('/',1)[1]
    reservations=aws('describe-instances',{'InstanceIds':[instance_id]})['Reservations']
    if len(reservations)!=1 or len(reservations[0]['Instances'])!=1:raise RuntimeError('ambiguous EC2 result')
    instance=reservations[0]['Instances'][0]
    image=s['windowsGpuImageBuilds' if a.variant=='gpu' else 'windowsImageBuilds'][a.windows_version]
    expected=image['imageID']
    if instance['VpcId']!=s['vpcID'] or {t['Key']:t['Value'] for t in instance.get('Tags',[])}.get('cloud-provisioning-test')!=s['runID']:
        raise RuntimeError('EC2 ownership mismatch')
    if instance.get('Platform')!='windows' or instance['ImageId']!=expected or len(instance['NetworkInterfaces'])!=1:
        raise RuntimeError('EC2 Windows image/single-ENI contract mismatch')
    report={'observedAt':datetime.datetime.now(datetime.timezone.utc).isoformat(),'machine':a.name,'instanceID':instance_id,
            'machineUID':machine['metadata']['uid'],'eniID':instance['NetworkInterfaces'][0]['NetworkInterfaceId'],
            'instanceLaunchTime':instance.get('LaunchTime'),'machineCreatedAt':machine['metadata'].get('creationTimestamp'),
            'windowsVersion':a.windows_version,'imageVariant':a.variant,'siteKubeletVersions':site_versions,'imageID':expected,'ec2State':instance['State']['Name'],'eniCount':1,
            'machineConditions':machine.get('status',{}).get('conditions',[])}
    guest=subprocess.run([str(a.awsnode.resolve()),'-work-dir',str(a.work_dir.resolve()),'-instance-id',instance_id,
                          '--','powershell.exe','-NoProfile','-NonInteractive','-Command',GUEST],capture_output=True,text=True,timeout=360)
    if guest.returncode:
        record_guest_failure(report,a.output,guest)
    else:report['guest']=json.loads(guest.stdout.lstrip('\ufeff'))
    ref=machine.get('status',{}).get('nodeRef',{})
    if ref.get('name'):
        node=kube('get','node',ref['name'])
        conditions=node.get('status',{}).get('conditions',[])
        report['node']={'name':node['metadata']['name'],'uid':node['metadata']['uid'],
                        'providerID':node['spec'].get('providerID'),'labels':node['metadata'].get('labels',{}),
                        'nodeInfo':node.get('status',{}).get('nodeInfo',{}),'conditions':conditions,'taints':node['spec'].get('taints',[]),
                        'associationMatches':associated(machine,node)}
        pods=kube('-n','cloud-provisioning','get','pods','--field-selector=spec.nodeName='+ref['name'])
        report['publisherPods']=[{'name':pod['metadata']['name'],'phase':pod.get('status',{}).get('phase'),
                                  'conditions':pod.get('status',{}).get('conditions',[])} for pod in pods['items']
                                  if not pod['metadata'].get('deletionTimestamp') and any(c['name']=='peer-publisher' for c in pod['spec']['containers'])]
        system=kube('-n','kube-system','get','pods','--field-selector=spec.nodeName='+ref['name'])
        report['systemPods']=[{'name':v['metadata']['name'],'phase':v.get('status',{}).get('phase'),'conditions':v.get('status',{}).get('conditions',[])} for v in system['items']]
    g=report.get('guest',{});n=report.get('node',{})
    checks={
        'expectedWindowsBuild':g.get('build')=={'2022':20348,'2025':26100}[a.windows_version],
        'onePhysicalNIC':len(g.get('physicalNICs',[]))==1,
        'k0sWorkerRunning':any(v['name']=='k0sworker' and v['status']=='Running' for v in g.get('services',[])),
        'tunnelRunning':any(v['name'].startswith('cloud-provisioning-cldt') and v['status']=='Running' for v in g.get('services',[])),
        'bootstrapInstalled':any(v['installed'] for v in g.get('meshes',[])),
        'capiAssociation':n.get('associationMatches',False),
        'windowsNode':n.get('labels',{}).get('kubernetes.io/os')=='windows',
        'matchesSiteKubelet':len(site_versions)==1 and n.get('nodeInfo',{}).get('kubeletVersion')==site_versions[0],
        'containerd2':n.get('nodeInfo',{}).get('containerRuntimeVersion','').startswith('containerd://2.'),
        'cloudWorkerLabel':n.get('labels',{}).get('cloud-provisioning.appmana.com/role')=='cloud-worker',
        'internetFacingTaint':any(v.get('key')=='cloud-provisioning.appmana.com/internet-facing' and v.get('effect')=='NoSchedule' for v in n.get('taints',[])),
        'nodeReady':any(v['type']=='Ready' and v['status']=='True' for v in n.get('conditions',[])),
        'publisherReady':any(any(c['type']=='Ready' and c['status']=='True' for c in v['conditions']) for v in report.get('publisherPods',[])),
    }
    if a.require_bootstrap_completion:
        result=subprocess.run([str(a.awsnode.resolve()),'-work-dir',str(a.work_dir.resolve()),'-instance-id',instance_id,'-bootstrap-status','-bootstrap-wait','15m','-timeout','16m'],capture_output=True,text=True)
        observation=None
        try: observation=json.loads(result.stdout)
        except ValueError: pass
        report['bootstrapCompletion']={'exitCode':result.returncode,'observation':observation}
        if result.stderr:
            diagnostic=a.output.with_suffix('.bootstrap.stderr')
            diagnostic.write_text(result.stderr)
            report['bootstrapCompletion']['privateErrorFile']=diagnostic.name
        checks['bootstrapCompleted']=result.returncode==0 and isinstance(observation,dict) and observation.get('complete') is True and observation.get('instanceID')==instance_id
    report['checks']=checks
    checks.update(image_artifacts(image,g,a.expected_running_tunnel_sha256))
    report['expectedRunningTunnelSHA256']=a.expected_running_tunnel_sha256 or image.get('tunnelSHA256')
    if a.require_peer_delivery:
        secret=kube('-n','cloud-provisioning','get','secret',a.name+'-tunnel-peers')
        checks['peerDelivery']=any(delivered(secret,mesh,datetime.datetime.now(datetime.timezone.utc)) for mesh in g.get('meshes',[]))
    if a.require_gateway:
        attachments=kube('-n','cloud-provisioning','get','configmaps')
        checks['gatewayAttachmentReady']=gateway_ready(attachments,machine['metadata']['uid'],n.get('uid'))
    report['scope']='join checkpoint only; no traffic, peer-update, replacement or CNI isolation claim'
    a.output.parent.mkdir(parents=True,exist_ok=True)
    a.output.write_text(json.dumps(report,indent=2)+'\n')
    print(json.dumps({'name':a.name,'checks':checks,'evidence':str(a.output)}))
    if not all(checks.values()):raise SystemExit(1)

if __name__=='__main__':main()
