"""Correlate a failed Windows container with captured HCS events.

This reports startup stages, not a root cause or a GPU qualification result.
Input events use the native collector's at/id/message fields. Raw HCS messages
can contain workload environment values; output deliberately excludes them.
"""
import datetime
import re


def timeline(pod, events, container_name):
    statuses = pod.get('status', {}).get('containerStatuses', [])
    matches = [s for s in statuses if s.get('name') == container_name]
    if len(matches) != 1:
        raise ValueError('Require one named container status')
    status = matches[0]
    container_id = status.get('containerID', '')
    if not re.fullmatch(r'containerd://[0-9a-f]{64}', container_id):
        raise ValueError('Require a containerd container identity')
    identity = container_id.removeprefix('containerd://')
    stages = []
    for event in events:
        message = event.get('message', '')
        if not message.startswith('[' + identity + '] '):
            continue
        stage = None
        if event.get('id') == 2022 and 'Container started with Silo Container ID' in message:
            stage = 'containerStarted'
        elif event.get('id') == 2500 and 'Create process,' in message:
            stage = 'createProcess'
        elif event.get('id') in (2000, 2001, 2003):
            stage = {2000: 'createSystem', 2001: 'startSystem', 2003: 'terminateSystem'}[event['id']]
        if stage is None:
            continue
        at = datetime.datetime.fromisoformat(event['at'].replace('Z', '+00:00'))
        if at.tzinfo is None:
            raise ValueError('Require timezone-aware HCS event timestamps')
        code = re.search(r'result (0x[0-9a-fA-F]{8})', message)
        result = code.group(1).lower() if code else None
        stages.append({'at': at.astimezone(datetime.timezone.utc).isoformat(), 'stage': stage, 'result': result,
                       'operationPending': result == '0xc0370103'})
    stages.sort(key=lambda e: e['at'])
    return {'podUID': pod['metadata']['uid'], 'node': pod['spec']['nodeName'],
            'containerID': identity, 'terminalReason': status.get('state', {}).get('terminated', {}).get('reason'),
            'stages': stages, 'scope': 'Identity-bound HCS startup timeline; no root-cause or GPU-success inference'}


def main():
    import argparse
    import json
    import pathlib
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['pod', 'events', 'output']:
        parser.add_argument('--' + name, type=pathlib.Path, required=True)
    parser.add_argument('--container', required=True)
    args = parser.parse_args()
    result = timeline(json.loads(args.pod.read_text()), json.loads(args.events.read_text()), args.container)
    with args.output.open('x') as output:
        json.dump(result, output, indent=2)
        output.write('\n')


if __name__ == '__main__':
    main()
