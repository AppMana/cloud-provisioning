"""Wait for a fresh Windows GPU Node, then create one Job without retrying creation.

Use a new private evidence directory per trial. A failed or ambiguous create must
be investigated using that Job name; rerunning with a different name invalidates
first-application evidence. This command neither provisions nor repairs a VM.
"""
import argparse
import datetime
import json
import os
import pathlib
import subprocess
import time

from windows_gpu import GPU, validate_image
from windows_plugin_placement import evaluate as placement
from windows_startup import evaluate as startup


def validate_job(job, node_name):
    metadata = job.get('metadata', {})
    spec = job.get('spec', {})
    pod = spec.get('template', {}).get('spec', {})
    containers = pod.get('containers', [])
    if (job.get('apiVersion') != 'batch/v1' or job.get('kind') != 'Job'
            or not metadata.get('name') or not metadata.get('namespace')
            or metadata.get('generateName') or metadata.get('uid') or 'status' in job
            or spec.get('backoffLimit') != 0 or spec.get('completions', 1) != 1
            or spec.get('parallelism', 1) != 1 or spec.get('suspend', False)
            or 'ttlSecondsAfterFinished' in spec or 'podFailurePolicy' in spec
            or pod.get('restartPolicy') != 'Never'
            or pod.get('nodeSelector', {}).get('kubernetes.io/hostname') != node_name
            or pod.get('nodeSelector', {}).get('kubernetes.io/os') != 'windows'
            or pod.get('nodeName') or pod.get('hostNetwork')
            or pod.get('initContainers') or pod.get('ephemeralContainers')
            or len(containers) != 1):
        raise ValueError('Require a named, retained, single-attempt Windows GPU Job targeting this Node')
    container = containers[0]
    contexts = [pod.get('securityContext', {}), container.get('securityContext', {})]
    if (any(c.get('windowsOptions', {}).get('hostProcess') for c in contexts)
            or str(container.get('resources', {}).get('limits', {}).get(GPU)) != '1'):
        raise ValueError('Require one ordinary container with exactly one WDDM device')
    validate_image(container.get('image', ''))


def qualify(node, pods, daemonsets, node_uid):
    first = startup(node, pods, node_uid, require_gpu=True)
    profiles = placement({'nodes': {'items': [node]}, 'pods': pods, 'daemonsets': daemonsets})
    return {'passed': first['passed'] and profiles['passed'],
            'startup': first, 'placement': profiles}


def submit(job, node_name, node_uid, evidence, get, create, timeout=900, interval=5):
    """Inject only API I/O for tests; all native attempts retain the same evidence."""
    validate_job(job, node_name)
    evidence.mkdir(mode=0o700)  # Refuse reuse, including after a create timeout.
    def save(name, value):
        with (evidence/name).open('x') as out:
            json.dump(value, out, indent=2); out.write('\n')
    save('intent.json', {'job': job, 'nodeName': node_name, 'nodeUID': node_uid})
    deadline = time.monotonic() + timeout
    attempt = 0
    while True:
        node = get('node', node_name, None)
        pods = get('pods', None, None)
        daemonsets = get('daemonsets', None, 'cldt-windows-gpu')
        result = qualify(node, pods, daemonsets, node_uid)
        save(f'observation-{attempt:04d}.json', {'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'node': node, 'pods': pods, 'daemonsets': daemonsets, 'result': result})
        checks = result['startup']['checks']
        if not checks['sameNode'] or not checks['windowsNode'] or not checks['noPriorOrdinaryContainers']:
            raise ValueError('First-application identity or inventory invalidated; no Job submitted')
        if result['passed']:
            break
        if time.monotonic() >= deadline:
            raise TimeoutError('Readiness deadline exceeded; no Job submitted')
        time.sleep(interval)
        attempt += 1
    # A Node replacement between snapshots must not receive this experiment.
    latest = get('node', node_name, None)
    latest_pods = get('pods', None, None)
    latest_sets = get('daemonsets', None, 'cldt-windows-gpu')
    final = qualify(latest, latest_pods, latest_sets, node_uid)
    save('node-before-create.json', latest)
    save('guard-before-create.json', {'pods': latest_pods, 'daemonsets': latest_sets, 'result': final})
    if not final['passed']:
        raise ValueError('Node, inventory or plugin readiness changed before create; no Job submitted')
    save('create-attempt.json', {'jobName': job['metadata']['name'], 'namespace': job['metadata']['namespace'],
        'at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
        'scope': 'One create attempt; snapshot observations cannot exclude concurrent or deleted workloads'})
    try:
        created = create(job)
    except Exception as error:
        save('create-unresolved.json', {'errorType': type(error).__name__,
            'action': 'Inspect this exact Job name and API state; never retry creation automatically'})
        raise
    save('created-job.json', created)
    return created


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ['api-server', 'bastion', 'node', 'node-uid']:
        parser.add_argument('--'+flag, required=True)
    for flag in ['job', 'evidence']:
        parser.add_argument('--'+flag, type=pathlib.Path, required=True)
    parser.add_argument('--timeout', type=int, default=900, help='readiness deadline in seconds')
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error('timeout must be positive')
    os.umask(0o077)
    command = ['docker', 'exec', '-i', args.bastion, 'kubectl', '--server='+args.api_server]
    def get(kind, name, namespace):
        flags = ['-n', namespace] if namespace else ([] if name else ['-A'])
        return json.loads(subprocess.check_output(command+['get', kind]+([name] if name else [])+flags+['-o', 'json'], timeout=30))
    def create(job):
        result = subprocess.run(command+['create', '-f', '-', '-o', 'json'], input=json.dumps(job),
            capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError('Job create failed; inspect the exact Job name before further action')
        return json.loads(result.stdout)
    created = submit(json.loads(args.job.read_text()), args.node, args.node_uid, args.evidence,
                     get, create, timeout=args.timeout)
    print(json.dumps({'job': created['metadata']['name'], 'jobUID': created['metadata']['uid'],
                      'scope': 'Submission only; first-process and GPU result observations remain required'}))


if __name__ == '__main__':
    main()
