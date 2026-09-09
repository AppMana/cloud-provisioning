#!/usr/bin/env python3
"""Verify projected-token rotation and a fresh Windows publisher acknowledgement.

Uses native SSM to read only JWT timestamps from the kubelet's projected volume.
Clears only the current peer acknowledgement, then waits for the real publisher
to restore it. Run after the publisher has lived through one requested lifetime.
"""
import argparse
import base64
import datetime
import hashlib
import json
import pathlib
import subprocess
import time

ANNOTATION = 'cloud-provisioning.appmana.com/applied'


def timestamp(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()


def rotated(container, metadata, now, lifetime):
    start = timestamp(container['state']['running']['startedAt'])
    return (container['restartCount'] == 0 and now - start > lifetime
            and start < metadata['issuedAt'] <= now < metadata['expiresAt'])


def acknowledged(before, after):
    payload = before['data']['peers.json']
    digest = hashlib.sha256(base64.b64decode(payload, validate=True)).hexdigest()
    return (before['metadata']['uid'] == after['metadata']['uid']
            and before['metadata']['resourceVersion'] != after['metadata']['resourceVersion']
            and after['data']['peers.json'] == payload
            and after['metadata'].get('annotations', {}).get(ANNOTATION) == digest)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir', required=True, type=pathlib.Path)
    p.add_argument('--awsnode', required=True, type=pathlib.Path)
    p.add_argument('--api-server', required=True)
    p.add_argument('--bastion', default='clab-cldt-bastion')
    p.add_argument('--name', required=True, help='CAPI Machine name')
    p.add_argument('--kubelet-root', default=r'C:\var\lib\k0s\kubelet')
    p.add_argument('--output', required=True, type=pathlib.Path)
    a = p.parse_args()
    if a.output.exists():
        raise RuntimeError('use a new evidence path')
    inventory = json.loads((a.work_dir / 'resources.json').read_text())
    if inventory.get('cleanedUp'):
        raise RuntimeError('run is cleaned up')
    k = ['docker', 'exec', a.bastion, 'kubectl', '--server='+a.api_server,
         '-n', 'cloud-provisioning']

    def kube(*args):
        result = subprocess.run(k + list(args) + ['-o', 'json'], capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError('Kubernetes observation or acknowledgement reset failed')
        return json.loads(result.stdout)

    machine = kube('get', 'machine', a.name)
    if machine['spec']['clusterName'] != inventory['runID']:
        raise RuntimeError('Machine belongs to another run')
    provider = machine['spec'].get('providerID', '')
    if not provider.startswith('aws:///'):
        raise RuntimeError('Machine has no AWS provider identity')
    node_name = machine['status']['nodeRef']['name']
    node = kube('get', 'node', node_name)
    if node['spec'].get('providerID') != provider or node['metadata']['labels'].get('kubernetes.io/os') != 'windows':
        raise RuntimeError('Windows Node association mismatch')
    pods = kube('get', 'pods', '-l', 'app=cloud-provisioning-dialer-remote-windows',
                '--field-selector=spec.nodeName='+node_name)['items']
    pods = [v for v in pods if not v['metadata'].get('deletionTimestamp')]
    if len(pods) != 1:
        raise RuntimeError('expected one live Windows publisher')
    pod = pods[0]
    container = next(v for v in pod['status']['containerStatuses'] if v['name'] == 'peer-publisher')
    spec = next(v for v in pod['spec']['containers'] if v['name'] == 'peer-publisher')
    mount = next(v for v in spec['volumeMounts'] if v['mountPath'] == r'C:\publisher-credentials')
    volume = next(v for v in pod['spec']['volumes'] if v['name'] == mount['name'])
    projection = next(v['serviceAccountToken'] for v in volume['projected']['sources'] if 'serviceAccountToken' in v)
    lifetime = projection['expirationSeconds']
    token_path = str(pathlib.PureWindowsPath(a.kubelet_root) / 'pods' / pod['metadata']['uid'] /
                     'volumes' / 'kubernetes.io~projected' / volume['name'] / projection['path'])
    script = r"""$ErrorActionPreference='Stop'
$t=[IO.File]::ReadAllText('__PATH__');$part=$t.Split('.')[1].Replace('-','+').Replace('_','/')
$part=$part.PadRight($part.Length+((4-$part.Length%4)%4),'=')
$j=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($part)) | ConvertFrom-Json
@{issuedAt=$j.iat;expiresAt=$j.exp} | ConvertTo-Json -Compress
""".replace('__PATH__', token_path.replace("'", "''"))
    result = subprocess.run([str(a.awsnode.resolve()), '-work-dir', str(a.work_dir.resolve()),
                             '-instance-id', provider.rsplit('/', 1)[1], '--', 'powershell.exe',
                             '-NoProfile', '-NonInteractive', '-Command', script],
                            capture_output=True, text=True, timeout=180)
    if result.returncode:
        raise RuntimeError('native projected-token timestamp observation failed')
    metadata = json.loads(result.stdout.lstrip('\ufeff'))
    if not rotated(container, metadata, time.time(), lifetime):
        raise RuntimeError('token rotation without restart is not yet established')
    name = a.name + '-tunnel-peers'
    before = kube('get', 'secret', name)
    # Both tests execute atomically with the removal. A concurrent publisher
    # update or Secret replacement must cause a conflict, not erase its ack.
    patch = [{'op': 'test', 'path': '/metadata/uid', 'value': before['metadata']['uid']},
             {'op': 'test', 'path': '/metadata/resourceVersion', 'value': before['metadata']['resourceVersion']},
             {'op': 'remove', 'path': '/metadata/annotations/'+ANNOTATION.replace('/', '~1')}]
    kube('patch', 'secret', name, '--type=json', '-p', json.dumps(patch))
    deadline = time.monotonic() + 60
    while True:
        after = kube('get', 'secret', name)
        if after['metadata']['uid'] != before['metadata']['uid'] or after['data']['peers.json'] != before['data']['peers.json']:
            raise RuntimeError('peer Secret identity or payload changed; repeat with a fresh baseline')
        if acknowledged(before, after):
            break
        if time.monotonic() >= deadline:
            raise RuntimeError('publisher did not acknowledge the unchanged payload after token rotation')
        time.sleep(2)
    current = kube('get', 'pod', pod['metadata']['name'])
    status = next(v for v in current['status']['containerStatuses'] if v['name'] == 'peer-publisher')
    if (current['metadata']['uid'] != pod['metadata']['uid'] or status['containerID'] != container['containerID']
            or status['restartCount'] != 0 or not status.get('ready')):
        raise RuntimeError('publisher changed or became unready during observation')
    report = {'scope': 'Projected-token rotation and fresh authenticated publisher acknowledgement; no CNI or lifecycle claim',
              'node': node_name, 'podUID': pod['metadata']['uid'], 'requestedLifetimeSeconds': lifetime,
              'publisherStartedAt': container['state']['running']['startedAt'], 'tokenMetadata': metadata,
              'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'tokenRotated': True, 'freshAuthenticatedAcknowledgement': True, 'restarts': 0}
    a.output.parent.mkdir(parents=True, exist_ok=True)
    with a.output.open('x') as out:
        out.write(json.dumps(report, indent=2)+'\n')
    print(node_name, 'projected token rotation and fresh acknowledgement verified')


if __name__ == '__main__':
    main()
