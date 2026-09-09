#!/usr/bin/env python3
"""Validate bounded AWS lifecycle evidence and export a credential-free summary."""
import argparse
import hashlib
import json
import re
from pathlib import Path


def read(path):
    return json.loads(path.read_text())


def row(path, expected):
    matrix = read(path / 'matrix.json')
    bindings = read(path / 'bindings.json')
    if (matrix['total'], matrix['passed'], matrix['failed']) != (expected, expected, 0):
        raise ValueError(f'{path.name}: matrix did not pass all {expected} checks')
    if len(matrix['results']) != expected or not all(r['passed'] for r in matrix['results']):
        raise ValueError(f'{path.name}: incomplete result evidence')
    if len(bindings) != (2 if expected == 140 else 1):
        raise ValueError(f'{path.name}: wrong number of AWS workers')
    nodes = {'cp', 'cp2', 'cp3', 'w1', 'w2'} | {b['node'] for b in bindings}
    paths = {(source, target, kind) for source in nodes for target in nodes
             if source != target for kind in ('pod', 'service', 'transfer')}
    paths |= {(source, '', kind) for source in nodes for kind in ('dns', 'external')}
    observed = {(r['from'], r['to'], r['kind']) for r in matrix['results']}
    if observed != paths or len(observed) != expected:
        raise ValueError(f'{path.name}: missing, duplicate or unexpected network paths')
    if any(not b['nodeUID'] or not b['providerID'].endswith('/'+b['instanceID']) for b in bindings):
        raise ValueError(f'{path.name}: incomplete machine identity')
    if len({b['instanceID'] for b in bindings}) != len(bindings):
        raise ValueError(f'{path.name}: duplicate instance identity')
    if any(b['eniCount'] != 1 or not b['physicalNIC'] for b in bindings):
        raise ValueError(f'{path.name}: missing single-NIC evidence')
    return {
        'row': path.name, 'total': expected, 'passed': expected, 'failed': 0,
        'matrixSHA256': hashlib.sha256((path / 'matrix.json').read_bytes()).hexdigest(),
        'bindings': bindings,
    }


def observed_images(path, versions):
    expected = {
        'calico-node': 'quay.io/k0sproject/calico-node:' + versions['calico'],
        'capa-controller-manager': 'registry.k8s.io/cluster-api-aws/cluster-api-aws-controller:v2.12.1',
        'capi-controller-manager': 'registry.k8s.io/cluster-api/cluster-api-controller:v1.11.1',
    }
    snapshots = list((path / 'observations').glob('ready-*.json'))
    if len(snapshots) != 1:
        raise ValueError('expected one ready observation snapshot')
    found = {}
    node_versions = set()
    for command in read(snapshots[0])['Commands']:
        if command['Node'] != 'api' or len(command['Args']) < 2 or command.get('Error'):
            continue
        if command['Args'][:2] == ['get', 'nodes']:
            node_versions = {node['status']['nodeInfo']['kubeletVersion']
                             for node in json.loads(command['Output'])['items']}
        if command['Args'][1] in ('daemonsets', 'deployments'):
            for obj in json.loads(command['Output'])['items']:
                name = obj['metadata']['name']
                if name in expected:
                    images = [c['image'] for c in obj['spec']['template']['spec']['containers']]
                    if expected[name] not in images:
                        raise ValueError('observed CNI or CAPI image differs from the pinned profile')
                    found[name] = expected[name]
    expected_kubelet = versions['k0s'].split('+')[0] + '+k0s'
    if node_versions != {expected_kubelet}:
        raise ValueError('observed node versions differ from the selected k0s profile')
    if found != expected:
        raise ValueError('missing observed distribution or provider image')
    return found


def summarize(work, baseline, placements, versions=None):
    versions = versions or {'k0s': 'v1.34.1+k0s.0', 'calico': 'v3.29.6-0', 'capa': 'v2.12.1', 'capi': 'v1.11.1'}
    state = read(work / 'resources.json')
    before = row(work / 'rows' / baseline, 140)
    rows = [before]
    replacements = []
    for index in (1, 2):
        removal = read(work / f'remove-{index}.json')
        survivor = row(work / 'rows' / f'survivor-{index}', 102)
        after = row(work / 'rows' / f'readded-{index}', 140)
        name = f'aws-k0s-{index}'
        old = {b['machine']: b for b in before['bindings']}
        new = {b['machine']: b for b in after['bindings']}
        if set(old) != {'aws-k0s-1', 'aws-k0s-2'} or set(new) != set(old):
            raise ValueError('lifecycle claim membership changed unexpectedly')
        if not removal['terminated'] or not removal['productCleanup'] or removal['claim'] != name:
            raise ValueError('removal did not confirm instance and product cleanup')
        for key in ('instanceID', 'nodeUID', 'providerID', 'node'):
            if removal[key] != old[name][key]:
                raise ValueError(f'{name}: removed identity differs from baseline')
        for key in ('instanceID', 'nodeUID'):
            if old[name][key] == new[name][key]:
                raise ValueError(f'{name}: readdition reused {key}')
        remaining = next(n for n in old if n != name)
        if survivor['bindings'] != [old[remaining]] or new[remaining] != old[remaining]:
            raise ValueError('surviving worker changed identity')
        rows.extend([survivor, after])
        replacements.append({'claim': name, 'before': old[name], 'after': new[name], 'removal': removal})
        before = after
    for placement in placements:
        path = work / 'rows' / ('placement-' + placement)
        measured = row(path, 140)
        if read(path / 'placement.json')['Name'] != placement:
            raise ValueError('placement evidence mismatch')
        if measured['bindings'] != before['bindings']:
            raise ValueError('placement changed worker identities')
        measured['placement'] = placement
        rows.append(measured)
    return {
        'runID': state['runID'], 'region': state['region'],
        'versions': versions,
        'network': 'bundled Calico, default VXLAN',
        'observedImages': observed_images(work / 'rows' / baseline, versions),
        'topology': {'siteVMs': 5, 'awsWorkers': 2, 'physicalNICsPerMachine': 1},
        'baseAMIID': state['baseAMIID'], 'preparedAMIID': state['preparedAMIID'],
        'gates': len(rows), 'checksPassed': sum(r['passed'] for r in rows),
        'rows': rows, 'replacements': replacements,
        'scope': 'AWS add/remove/readd and listed endpoint placements; no EC2 power or physical-NIC outage claim',
        'resourcesCleanedUp': state.get('cleanedUp', False),
        'cleanupVerification': read(work / 'cleanup-verification.json') if state.get('cleanedUp') else None,
    }


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir', required=True, type=Path)
    p.add_argument('--baseline', required=True)
    p.add_argument('--placements', default='control-plane,one-worker,two-workers,all-nodes')
    p.add_argument('--output', required=True, type=Path)
    p.add_argument('--unit-coverage-log', type=Path)
    p.add_argument('--runtime-coverage', type=Path)
    p.add_argument('--dialer-binary', type=Path)
    p.add_argument('--expected-k0s', default='v1.34.1+k0s.0', help='release under test; historical default retained for old evidence')
    p.add_argument('--expected-calico', default='v3.29.6-0', help='expected distribution-bundled Calico image tag')
    a = p.parse_args()
    result = summarize(a.work_dir, a.baseline, [x for x in a.placements.split(',') if x],
                       {'k0s': a.expected_k0s, 'calico': a.expected_calico, 'capa': 'v2.12.1', 'capi': 'v1.11.1'})
    coverage = {}
    if a.unit_coverage_log:
        totals = re.findall(r'^total:.*?([0-9.]+)%$', a.unit_coverage_log.read_text(), re.M)
        if len(totals) != 2:
            raise ValueError('expected harness and controller totals from make coverage')
        coverage['unitStatementPercent'] = {'harness': float(totals[0]), 'controller': float(totals[1])}
    if a.runtime_coverage:
        values = re.findall(r'^\s*(\S+)\s+coverage:\s+([0-9.]+)%', a.runtime_coverage.read_text(), re.M)
        if not values:
            raise ValueError('expected go tool covdata percent output')
        coverage['runtimeStatementPercent'] = {
            package.removeprefix('github.com/appmana/cloud-provisioning/harness/e2e/'): float(value)
            for package, value in values}
    if coverage:
        result['coverage'] = coverage
    if a.dialer_binary:
        result['dialerBinarySHA256'] = hashlib.sha256(a.dialer_binary.read_bytes()).hexdigest()
    a.output.write_text(json.dumps(result, indent=2) + '\n')
    print(f"Verified {result['gates']} gates, {result['checksPassed']} checks and two replacements")
