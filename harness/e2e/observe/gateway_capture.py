"""Read-only, identity-bound capture of a staged gateway withdrawal.

Record public state hashes, never raw Secret data. Start before deleting the
original Machine; validate ordering separately from traffic and EC2 termination.
"""
import argparse
import base64
import datetime
import hashlib
import json
import pathlib
import subprocess
import time

from gateway_withdrawal import evaluate


def sample(get, binding):
    # Keep this order: the next mesh read brackets this row's acknowledgement.
    mesh = get('secret', 'cloud-provisioning-peers', 'cloud-provisioning')
    secret = get('secret', binding['node'] + '-tunnel-peers', 'cloud-provisioning')
    machine = get('machine', binding['machine'], 'cloud-provisioning')
    node = get('node', binding['node'], None)
    if not mesh or mesh['metadata']['uid'] != binding['meshUID']:
        raise ValueError('Original mesh is absent or replaced')
    for obj, uid, kind in [(secret, binding['secretUID'], 'Secret'),
                           (machine, binding['machineUID'], 'Machine'),
                           (node, binding['nodeUID'], 'Node')]:
        if obj and obj['metadata']['uid'] != uid:
            raise ValueError('Original ' + kind + ' was replaced')
    plans = json.loads(base64.b64decode(mesh['data'].get('gateway-projections.json', 'W10='), validate=True))
    matching = [plan for plan in plans if plan['lease'] == binding['lease']]
    if len(matching) > 1:
        raise ValueError('Ambiguous duplicate gateway lease')
    plan = matching[0] if matching else None
    return dict(
        at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
        meshUID=mesh['metadata']['uid'], meshResourceVersion=mesh['metadata']['resourceVersion'],
        projectionPresent=plan is not None, retiringWorker=bool(plan and plan.get('retiringWorker')),
        workerSecretUID=secret['metadata']['uid'] if secret else None,
        workerDocumentSHA256=hashlib.sha256(base64.b64decode(secret['data']['peers.json'], validate=True)).hexdigest() if secret else None,
        workerAppliedSHA256=secret['metadata'].get('annotations', {}).get('cloud-provisioning.appmana.com/applied') if secret else None,
        machinePresent=machine is not None,
        machineDeleting=bool(machine and machine['metadata'].get('deletionTimestamp')),
        nodeReady=bool(node and any(c['type'] == 'Ready' and c['status'] == 'True'
                                   for c in node.get('status', {}).get('conditions', []))),
    )


def capture(get, binding, output, deadline, interval=1):
    required = ['machine', 'machineUID', 'node', 'nodeUID', 'meshUID', 'secretUID', 'lease']
    if any(not isinstance(binding.get(key), str) or not binding[key] for key in required):
        raise ValueError('Explicit original identities and gateway lease required')
    output = pathlib.Path(output)
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    (output / 'binding.json').write_text(json.dumps({key: binding[key] for key in required}, indent=2))
    rows = []
    with (output / 'samples.jsonl').open('x') as log:
        while time.monotonic() < deadline:
            row = sample(get, binding)
            log.write(json.dumps(row) + '\n')
            log.flush()
            rows.append(row)
            if len(rows) == 1 and (not row['projectionPresent'] or row['retiringWorker'] or
                                  not row['machinePresent'] or row['machineDeleting'] or
                                  not row['nodeReady'] or row['workerSecretUID'] != binding['secretUID']):
                raise ValueError('Capture must begin on the original active, Ready attachment')
            if not row['machinePresent']:
                result = evaluate(rows, binding['secretUID'])
                (output / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
                return result
            time.sleep(min(interval, max(0, deadline - time.monotonic())))
    raise TimeoutError('Original observation deadline reached; retain samples and inspect the original deletion')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--api-server', required=True)
    parser.add_argument('--bastion', required=True)
    parser.add_argument('--binding', type=pathlib.Path, required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    parser.add_argument('--timeout', type=int, default=1200, help='Whole observation deadline in seconds')
    args = parser.parse_args()
    if not 1 <= args.timeout <= 7200:
        parser.error('--timeout must be between 1 and 7200 seconds')
    deadline = time.monotonic() + args.timeout
    command = ['docker', 'exec', args.bastion, 'kubectl', '--server=' + args.api_server]

    def get(kind, name, namespace):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError('Original observation deadline reached')
        invocation = command + (['-n', namespace] if namespace else [])
        result = subprocess.run(invocation + ['get', kind, name, '--ignore-not-found', '-o', 'json'],
                                capture_output=True, timeout=min(30, remaining))
        if result.returncode:
            # Do not include possibly credential-bearing command output.
            raise RuntimeError('Read-only Kubernetes observation failed for ' + kind)
        return json.loads(result.stdout) if result.stdout.strip() else None

    result = capture(get, json.loads(args.binding.read_text()), args.output, deadline)
    print(json.dumps(result, indent=2))
    raise SystemExit(0 if result['passed'] else 1)


if __name__ == '__main__':
    main()
