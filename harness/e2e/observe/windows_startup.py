"""Check a captured pre-application Pod inventory for a fresh Windows VM.

Call before every first-application submission, retaining snapshots from node
registration onward. This snapshot check cannot prove the absence of deleted
Pods or containers launched outside Kubernetes.
"""

import re

from windows_gpu import GPU


def evaluate(node, pods, node_uid, require_gpu=False):
    name = node['metadata']['name']
    ordinary = []
    for pod in pods['items']:
        if pod['spec'].get('nodeName') != name:
            continue
        spec = pod['spec']
        for kind in ['initContainers', 'containers', 'ephemeralContainers']:
            for container in spec.get(kind, []):
                options = dict(spec.get('securityContext', {}).get('windowsOptions', {}))
                options.update(container.get('securityContext', {}).get('windowsOptions', {}))
                if options.get('hostProcess') is not True:
                    ordinary.append({'pod': pod['metadata']['name'],
                        'podUID': pod['metadata']['uid'], 'container': container['name'],
                        'kind': kind, 'phase': pod.get('status', {}).get('phase')})
    checks = {
        'sameNode': bool(node_uid) and node['metadata']['uid'] == node_uid,
        'windowsNode': node['metadata'].get('labels', {}).get('kubernetes.io/os') == 'windows',
        'nodeReady': not node['metadata'].get('deletionTimestamp') and any(
            c['type'] == 'Ready' and c['status'] == 'True'
            for c in node.get('status', {}).get('conditions', [])),
        'noPriorOrdinaryContainers': not ordinary,
    }
    if require_gpu:
        quantities = [node.get('status', {}).get(field, {}).get(GPU, '')
                      for field in ['capacity', 'allocatable']]
        counts = [int(value) if isinstance(value, str) and re.fullmatch(r'[1-9][0-9]*', value)
                  else 0 for value in quantities]
        checks['gpuReportedAllocatable'] = 0 < counts[1] <= counts[0]
    return {'passed': all(checks.values()), 'checks': checks,
            'nodeUID': node['metadata']['uid'], 'ordinaryContainers': ordinary,
            'scope': 'Pre-application Kubernetes snapshot; does not establish absence of deleted Pods or out-of-band containers'}


def main():
    import argparse
    import json
    import pathlib
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ['node', 'pods', 'output']:
        parser.add_argument('--'+flag, required=True, type=pathlib.Path)
    parser.add_argument('--node-uid', required=True)
    parser.add_argument('--require-gpu', action='store_true',
                        help='also require reported WDDM capacity and allocatable resources')
    args = parser.parse_args()
    result = evaluate(json.loads(args.node.read_text()), json.loads(args.pods.read_text()), args.node_uid, require_gpu=args.require_gpu)
    with args.output.open('x') as output:
        json.dump(result, output, indent=2);output.write('\n')
    print(json.dumps(result))
    if not result['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
