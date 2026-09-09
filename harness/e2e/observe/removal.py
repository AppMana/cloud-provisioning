"""Compare native before/after snapshots for a CAPI worker removal.

The EC2 termination/product-cleanup receipt comes from cmd/awsremove. This
comparison adds survivor and gateway-lease checks; it does not issue deletions.
"""
import argparse
import json
import pathlib


def by_name(snapshot, kind):
    items = snapshot[kind]['items']
    result = {item['metadata']['name']: item for item in items}
    if len(result) != len(items):
        raise ValueError('Ambiguous duplicate names in ' + kind)
    return result


def ready(node):
    return (not node['metadata'].get('deletionTimestamp') and
            any(c['type'] == 'Ready' and c['status'] == 'True'
                for c in node.get('status', {}).get('conditions', [])))


def attachments(snapshot):
    return {name: (cm['metadata']['uid'], json.loads(cm['data']['record.json']))
            for name, cm in by_name(snapshot, 'configmaps').items()
            if name.startswith('network-attachment-')}



def preflight(snapshot, claim, machine_uid):
    """Qualify a Ready-attachment removal baseline before issuing deletion."""
    nodes = by_name(snapshot, 'nodes')
    machines = by_name(snapshot, 'machines')
    target = machines.get(claim, {})
    node_name = target.get('status', {}).get('nodeRef', {}).get('name')
    node = nodes.get(node_name, {})
    provider = target.get('spec', {}).get('providerID', '')
    leases = attachments(snapshot)
    target_leases = [(name, lease) for name, (_, lease) in leases.items()
                     if lease['plan']['worker']['uid'] == machine_uid
                     and lease['phase'] != 'Complete']
    other_leases = [(name, lease) for name, (_, lease) in leases.items()
                    if lease['plan']['worker']['uid'] != machine_uid
                    and lease['phase'] != 'Complete']
    checks = {
        'targetMachineIdentity': bool(machine_uid) and target.get('metadata', {}).get('uid') == machine_uid,
        'noDeletingMachines': bool(machines) and all(not m['metadata'].get('deletionTimestamp') for m in machines.values()),
        'nodeAssociation': bool(node.get('metadata', {}).get('uid')) and
            provider.startswith('aws:///') and bool(provider.rsplit('/', 1)[-1]) and
            node.get('spec', {}).get('providerID') == provider,
        'allNodesReady': bool(nodes) and all(ready(n) for n in nodes.values()),
        'targetGatewayLeasesReady': bool(target_leases) and all(lease['phase'] == 'Ready' and
            lease['plan']['worker'].get('nodeUID') == node.get('metadata', {}).get('uid')
            for _, lease in target_leases),
        'otherGatewayLeasesReady': bool(other_leases) and all(lease['phase'] == 'Ready' for _, lease in other_leases),
    }
    return {'passed': all(checks.values()), 'checks': checks, 'claim': claim,
            'machineUID': machine_uid, 'nodeUID': node.get('metadata', {}).get('uid'),
            'providerID': provider,
            'targetLeasePhases': {name: lease['phase'] for name, lease in target_leases},
            'otherLeasePhases': {name: lease['phase'] for name, lease in other_leases},
            'scope': 'Snapshot preflight for Ready-attachment removal with survivors; does not delete resources or authorize reuse after state changes'}


def evaluate(before, after, receipt):
    nodes = by_name(before, 'nodes')
    machines = by_name(before, 'machines')
    target = machines[receipt['claim']]
    node = nodes[receipt['node']]
    provider = target['spec']['providerID']
    if (node['metadata']['uid'] != receipt['nodeUID'] or
            target['status']['nodeRef']['name'] != receipt['node'] or
            node['spec']['providerID'] != provider or provider != receipt['providerID'] or
            not provider.startswith('aws:///') or provider.rsplit('/', 1)[-1] != receipt['instanceID']):
        raise ValueError('Removal receipt does not identify the baseline Machine and Node')
    if not all(ready(n) for n in nodes.values()):
        raise ValueError('Baseline contains unhealthy Nodes')
    survivors = {name: n for name, n in nodes.items() if name != receipt['node']}
    observed = by_name(after, 'nodes')
    remaining_machines = by_name(after, 'machines')
    survivor_machines = {name: m for name, m in machines.items() if name != receipt['claim']}
    original_leases = attachments(before)
    observed_leases = attachments(after)
    retired = []
    preserved = []
    for name, (uid, lease) in original_leases.items():
        if lease['phase'] != 'Ready':
            continue
        current_uid, current = observed_leases.get(name, (None, {}))
        same_identity = (current_uid == uid and all(current.get(k) == lease[k]
                         for k in ['id', 'lease', 'digest', 'plan']))
        if lease['plan']['worker']['uid'] == target['metadata']['uid']:
            retired.append(same_identity and current.get('phase') == 'Complete')
        else:
            preserved.append(same_identity and current.get('phase') == 'Ready')
    checks = {
        'instanceTerminated': receipt.get('terminated') is True,
        'productCleanup': receipt.get('productCleanup') is True,
        'targetMachineAbsent': receipt['claim'] not in remaining_machines,
        'survivorMachineIdentities': set(remaining_machines) == set(survivor_machines) and all(
            remaining_machines[name]['metadata']['uid'] == old['metadata']['uid'] and
            not remaining_machines[name]['metadata'].get('deletionTimestamp') and
            remaining_machines[name]['spec'].get('providerID') == old['spec'].get('providerID')
            for name, old in survivor_machines.items()),
        'exactSurvivorSet': set(observed) == set(survivors),
        'survivorIdentitiesAndReadiness': all(
            name in observed and ready(observed[name]) and
            observed[name]['metadata']['uid'] == old['metadata']['uid'] and
            observed[name].get('spec', {}).get('providerID') == old.get('spec', {}).get('providerID')
            for name, old in survivors.items()),
        'targetGatewayLeasesComplete': bool(retired) and all(retired),
        'otherGatewayLeasesPreserved': bool(preserved) and all(preserved),
    }
    return {'passed': all(checks.values()), 'checks': checks,
            'survivorCount': len(survivors), 'retiredGatewayLeaseCount': len(retired),
            'preservedGatewayLeaseCount': len(preserved),
            'scope': 'Snapshot identity/readiness and gateway lease preservation; application/network survivor probes are separate'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ['before', 'output']:
        parser.add_argument('--' + name, type=pathlib.Path, required=True)
    for name in ['after', 'receipt']:
        parser.add_argument('--' + name, type=pathlib.Path)
    parser.add_argument('--preflight', action='store_true')
    parser.add_argument('--claim')
    parser.add_argument('--machine-uid')
    args = parser.parse_args()
    if args.preflight:
        if not args.claim or not args.machine_uid or args.after or args.receipt:
            parser.error('preflight requires claim and machine-uid, without after or receipt')
        result = preflight(json.loads(args.before.read_text()), args.claim, args.machine_uid)
    else:
        if not args.after or not args.receipt or args.claim or args.machine_uid:
            parser.error('comparison requires after and receipt, without claim or machine-uid')
        result = evaluate(*(json.loads(getattr(args, name).read_text()) for name in ['before', 'after', 'receipt']))
    with args.output.open('x') as output:
        json.dump(result, output, indent=2)
        output.write('\n')
    print(json.dumps(result))
    if not result['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
