"""Observe one real Windows GPU application Job without provisioning or retrying it."""
import argparse
import base64
import hashlib
import datetime
import json
import pathlib
import re
import subprocess

from pod_matrix import Kubectl

GPU = 'directx.microsoft.com/display'
IMAGE = 'index.docker.io/tensorworks/example-ffmpeg@sha256:e3cc01b8396c6f562c7530ef38271db29cb65d4b70a4c2b8eb76ab35eda9defe'
COMMAND = ['powershell.exe', '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', r'C:\gpu-probe\run.ps1']


def validate_image(image):
    if not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', image):
        raise ValueError('Require an explicit digest-pinned workload image')
    return image


def evaluate(node, pod, job, node_uid, pod_uid, logs='', probe_name='windows-gpu-probe', image=IMAGE):
    validate_image(image)
    if node['metadata']['uid'] != node_uid or pod['metadata']['uid'] != pod_uid:
        raise ValueError('Node or Pod identity changed')
    if pod['spec'].get('nodeName') != node['metadata']['name']:
        raise ValueError('Pod is not on the expected Node')
    if not any(o.get('uid') == job['metadata']['uid'] and o.get('kind') == 'Job'
               and o.get('controller') is True for o in pod['metadata'].get('ownerReferences', [])):
        raise ValueError('Pod does not belong to the observed Job')
    containers = pod['spec']['containers']
    contexts = [pod['spec'].get('securityContext', {})] + [c.get('securityContext', {}) for c in containers]
    if (len(containers) != 1 or pod['spec'].get('hostNetwork') or pod['spec'].get('initContainers') or
            any(c.get('windowsOptions', {}).get('hostProcess') for c in contexts)):
        raise ValueError('Require one ordinary application container')
    container = containers[0]
    if (node['metadata'].get('labels', {}).get('kubernetes.io/os') != 'windows' or
            str(container.get('resources', {}).get('limits', {}).get(GPU)) != '1'):
        raise ValueError('Require a Windows workload requesting exactly one WDDM device')
    if container.get('image') != image or container.get('command') != COMMAND or container.get('args'):
        raise ValueError('Require the pinned probe image and command')
    if (pod['spec'].get('volumes') != [{'name': 'probe', 'configMap': {'name': probe_name, 'defaultMode': 420}}]
            or container.get('volumeMounts') != [{'name': 'probe', 'readOnly': True, 'mountPath': r'C:\gpu-probe'}]):
        raise ValueError('Require the immutable probe volume without alternate mounts')
    statuses = pod.get('status', {}).get('containerStatuses', [])
    current = next((c for c in statuses if c['name'] == container['name']), {})
    terminal = current.get('state', {}).get('terminated', {})
    checks = {
        'podSucceeded': pod.get('status', {}).get('phase') == 'Succeeded',
        'jobComplete': any(c['type'] == 'Complete' and c['status'] == 'True'
                           for c in job.get('status', {}).get('conditions', [])),
        'containerExitedZero': terminal.get('exitCode') == 0,
        'containerIdentityPresent': bool(current.get('containerID')) and bool(current.get('imageID')),
        'hardwareRenderAndEncode': False,
    }
    receipt = None
    for line in logs.splitlines():
        try:
            candidate = json.loads(line)
        except ValueError:
            continue
        if isinstance(candidate, dict) and 'render' in candidate:
            if receipt is not None:
                raise ValueError('Ambiguous multiple render receipts')
            receipt = candidate
    if receipt:
        render = receipt.get('render')
        if isinstance(render, dict):
            checks['hardwareRenderAndEncode'] = (
                render.get('api') == 'Direct3D11' and render.get('hardwareOnly') is True
                and render.get('vendorID') == 4318 and render.get('frames') == 30
                and render.get('pixelsPerFrame') == 4096
                and type(render.get('greenPixels')) is int and render['greenPixels'] >= 200
                and type(render.get('bluePixels')) is int and render['bluePixels'] >= 200
                and render['greenPixels'] + render['bluePixels'] == 4096
                and receipt.get('encoder') == 'h264_nvenc' and receipt.get('decodedFrames') == 30
                and type(receipt.get('encodedBytes')) is int and receipt['encodedBytes'] > 0)
    return {'passed': all(checks.values()), 'checks': checks, 'receipt': receipt,
            'podUID': pod_uid, 'nodeUID': node_uid, 'jobUID': job['metadata']['uid'],
            'podPhase': pod.get('status', {}).get('phase'), 'containerStatus': current,
            'image': container['image'], 'hostNodeInfo': node.get('status', {}).get('nodeInfo', {}),
            'scope': 'Single ordinary-container render and encode observation; no streaming or tenant-isolation claim'}



def write_observation(path, result):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open('x') as stream:
        json.dump(result, stream, indent=2)
        stream.write('\n')


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for flag in ['api-server', 'bastion', 'namespace', 'pod', 'pod-uid', 'node-uid', 'job']:
        p.add_argument('--' + flag, required=True)
    p.add_argument('--image', default=IMAGE, type=validate_image, help='expected digest-pinned workload image; defaults to the historical probe')
    p.add_argument('--probe-identity', type=pathlib.Path, required=True)
    p.add_argument('--output', type=pathlib.Path, required=True)
    a = p.parse_args()
    kube = Kubectl(a.api_server, a.bastion, a.namespace)
    identity = json.loads(a.probe_identity.read_text())
    probe_name = identity.get('configMapName', 'windows-gpu-probe')
    def verify_probe():
        cm = kube.get('configmap', probe_name)
        if (cm['metadata']['uid'] != identity['configMapUID'] or cm.get('immutable') is not True
                or hashlib.sha256(base64.b64decode(cm['binaryData']['render.exe'])).hexdigest() != identity['binarySHA256']
                or hashlib.sha256(cm['data']['run.ps1'].encode()).hexdigest() != identity['scriptSHA256']):
            raise ValueError('Immutable probe identity or bytes changed')
    verify_probe()
    pod = kube.get('pod', a.pod)
    node = kube.get('node', pod['spec']['nodeName'])
    job = kube.get('job', a.job)
    # Validate isolation and identity before fetching application logs.
    evaluate(node, pod, job, a.node_uid, a.pod_uid, probe_name=probe_name, image=a.image)
    logs = ''
    log_error = None
    if pod.get('status', {}).get('phase') in ['Succeeded', 'Failed']:
        result = subprocess.run(kube.command + ['logs', a.pod, '-c', pod['spec']['containers'][0]['name']],
                                capture_output=True, text=True, timeout=60)
        if result.returncode:
            log_error = result.stderr
        else:
            logs = result.stdout
    after_pod = kube.get('pod', a.pod)
    after_node = kube.get('node', node['metadata']['name'])
    after_job = kube.get('job', a.job)
    result = evaluate(after_node, after_pod, after_job, a.node_uid, a.pod_uid, logs, probe_name, image=a.image)
    verify_probe()
    result['probeIdentity'] = identity
    result['logReadError'] = log_error
    result.update(observedAt=datetime.datetime.now(datetime.timezone.utc).isoformat(), logs=logs)
    write_observation(a.output, result)
    print(json.dumps({k: result[k] for k in ['passed', 'podPhase', 'checks']}))
    if not result['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
