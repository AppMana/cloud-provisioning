#!/usr/bin/env python3
"""Validate bounded AWS lifecycle evidence and export a credential-free summary."""
import argparse
import hashlib
import json
import re
from pathlib import Path


def read(path):
    return json.loads(path.read_text())


DEFAULT_SITE = ('cp', 'cp2', 'cp3', 'w1', 'w2')


def expected_paths(nodes, dual=False):
    """Every ordered pair's reachability checks and every node's own."""
    pair_kinds = ('pod', 'service', 'transfer') + (('pod6', 'service6', 'transfer6') if dual else ())
    node_kinds = ('dns', 'external') + (('dns6',) if dual else ())
    paths = {(source, target, kind, 0) for source in nodes for target in nodes
             if source != target for kind in pair_kinds}
    paths |= {(source, '', kind, 0) for source in nodes for kind in node_kinds}
    return paths


def row(path, expected=None, site=DEFAULT_SITE, dual=False, udp=False, workers=None):
    """Check one row's matrix against the paths its profile requires.

    UDP rows add, for every ordered pair, one exact-echo check per datagram
    size in each family; the sizes follow the pods' measured MTU, so they
    are required to be the same for every pair and family rather than
    restated here.
    """
    matrix = read(path / 'matrix.json')
    bindings = read(path / 'bindings.json')
    if matrix['failed'] != 0 or matrix['passed'] != matrix['total']:
        raise ValueError(f'{path.name}: matrix did not pass all {matrix["total"]} checks')
    if len(matrix['results']) != matrix['total'] or not all(r['passed'] for r in matrix['results']):
        raise ValueError(f'{path.name}: incomplete result evidence')
    if workers is None:
        workers = 2 if expected == 140 else 1
    if len(bindings) != workers:
        raise ValueError(f'{path.name}: wrong number of AWS workers')
    nodes = set(site) | {b['node'] for b in bindings}
    paths = expected_paths(nodes, dual)
    keys = [(r['from'], r['to'], r['kind'], r.get('datagramBytes', 0)) for r in matrix['results']]
    observed = set(keys)
    if len(observed) != len(keys):
        raise ValueError(f'{path.name}: missing, duplicate or unexpected network paths')
    if udp:
        families = ('udp', 'udp6') if dual else ('udp',)
        sizes = {(f, t, k): set() for f in nodes for t in nodes if f != t for k in families}
        for f, t, k, size in observed:
            if k in families:
                if (f, t, k) not in sizes or size <= 0:
                    raise ValueError(f'{path.name}: missing, duplicate or unexpected network paths')
                sizes[(f, t, k)].add(size)
        distinct = {frozenset(v) for v in sizes.values()}
        if len(distinct) != 1 or not next(iter(distinct)):
            raise ValueError(f'{path.name}: UDP sizes differ between pairs or are missing')
        paths |= {(f, t, k, size) for (f, t, k), v in sizes.items() for size in v}
    if observed != paths:
        raise ValueError(f'{path.name}: missing, duplicate or unexpected network paths')
    if expected is not None and len(observed) != expected:
        raise ValueError(f'{path.name}: missing, duplicate or unexpected network paths')
    if any(not b['nodeUID'] or not b['providerID'].endswith('/'+b['instanceID']) for b in bindings):
        raise ValueError(f'{path.name}: incomplete machine identity')
    if len({b['instanceID'] for b in bindings}) != len(bindings):
        raise ValueError(f'{path.name}: duplicate instance identity')
    if any(b['eniCount'] != 1 or not b['physicalNIC'] for b in bindings):
        raise ValueError(f'{path.name}: missing single-NIC evidence')
    total = len(observed)
    return {
        'row': path.name, 'total': total, 'passed': total, 'failed': 0,
        'matrixSHA256': hashlib.sha256((path / 'matrix.json').read_bytes()).hexdigest(),
        'bindings': bindings,
    }


def observed_images(path, versions):
    expected = {
        'calico-node': versions.get('calicoImage') or 'quay.io/k0sproject/calico-node:' + versions['calico'],
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


def summarize(work, baseline, placements, versions=None, profile=None):
    versions = versions or {'k0s': 'v1.34.1+k0s.0', 'calico': 'v3.29.6-0', 'capa': 'v2.12.1', 'capi': 'v1.11.1'}
    profile = profile or {}
    site = profile.get('site', DEFAULT_SITE)
    shape = dict(site=site, dual=profile.get('dual', False), udp=profile.get('udp', False))
    # The historical five-node VXLAN row counts stay pinned; other
    # profiles are checked by their path set alone.
    both, one = (140, 102) if not profile else (None, None)
    state = read(work / 'resources.json')
    before = row(work / 'rows' / baseline, both, workers=2, **shape)
    rows = [before]
    replacements = []
    for index in (1, 2):
        removal = read(work / f'remove-{index}.json')
        survivor = row(work / 'rows' / f'survivor-{index}', one, workers=1, **shape)
        after = row(work / 'rows' / f'readded-{index}', both, workers=2, **shape)
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
        measured = row(path, both, workers=2, **shape)
        if read(path / 'placement.json')['Name'] != placement:
            raise ValueError('placement evidence mismatch')
        if measured['bindings'] != before['bindings']:
            raise ValueError('placement changed worker identities')
        measured['placement'] = placement
        rows.append(measured)
    return {
        'runID': state['runID'], 'region': state['region'],
        'versions': versions,
        'network': profile.get('network', 'bundled Calico, default VXLAN'),
        'dualStack': shape['dual'], 'udpSizeProbes': shape['udp'],
        'observedImages': observed_images(work / 'rows' / baseline, versions),
        'topology': {'siteVMs': len(site), 'awsWorkers': 2, 'physicalNICsPerMachine': 1},
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
    p.add_argument('--expected-calico-image', help='exact calico-node image reference, for profiles that pin by digest')
    p.add_argument('--site-nodes', help='comma-separated site nodes; selects profile-checked rows')
    p.add_argument('--dual-stack', action='store_true', help='rows carry IPv6 checks for every path')
    p.add_argument('--udp', action='store_true', help='rows carry UDP size probes')
    p.add_argument('--network', help='network description for the summary')
    a = p.parse_args()
    profile = None
    if a.site_nodes:
        profile = {'site': [n for n in a.site_nodes.split(',') if n], 'dual': a.dual_stack, 'udp': a.udp,
                   'network': a.network or 'distribution-bundled network'}
    versions = {'k0s': a.expected_k0s, 'calico': a.expected_calico, 'capa': 'v2.12.1', 'capi': 'v1.11.1'}
    if a.expected_calico_image:
        versions['calicoImage'] = a.expected_calico_image
    result = summarize(a.work_dir, a.baseline, [x for x in a.placements.split(',') if x], versions, profile)
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
