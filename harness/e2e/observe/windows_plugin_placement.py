"""Verify the test WDDM plugin profiles select Windows builds, not hostnames."""
import argparse
import datetime
import json
import pathlib
import subprocess

PROFILES = {'10.0.20348': 'windows-wddm-device-plugin',
            '10.0.26100': 'windows-wddm-device-plugin-runtime-mounts'}
LABEL = 'cloud-provisioning.appmana.com/gpu-test'


def evaluate(snapshot):
    sets = {d['metadata']['name']: d for d in snapshot['daemonsets']['items']}
    nodes = [n for n in snapshot['nodes']['items']
             if n['metadata']['labels'].get(LABEL) == 'true'
             and n['metadata']['labels'].get('kubernetes.io/os') == 'windows']
    checks = {'profilesPresent': all(name in sets for name in PROFILES.values()),
              'buildSelectors': True, 'windows2025RuntimeMountsSkipped': False,
              'supportedNodesPresent': bool(nodes), 'oneReadyPluginPerNode': True}
    for build, name in PROFILES.items():
        spec = sets.get(name, {}).get('spec', {}).get('template', {}).get('spec', {})
        selector = spec.get('nodeSelector', {})
        checks['buildSelectors'] &= (selector == {'kubernetes.io/os': 'windows',
            'kubernetes.io/arch': 'amd64', 'node.kubernetes.io/windows-build': build,
            LABEL: 'true'} and not spec.get('affinity'))
        if build == '10.0.26100':
            cs = spec.get('containers', [])
            checks['windows2025RuntimeMountsSkipped'] = (len(cs) == 1 and
                {'name': 'WDDM_DEVICE_PLUGIN_SKIP_RUNTIME_MOUNTS', 'value': 'true'} in cs[0].get('env', []))
    assignments = []
    for node in nodes:
        name = node['metadata']['name'];build = node['metadata']['labels'].get('node.kubernetes.io/windows-build')
        expected = PROFILES.get(build); ds = sets.get(expected, {})
        pods = [p for p in snapshot['pods']['items'] if p['spec'].get('nodeName') == name
                and any(o.get('kind') == 'DaemonSet' and o.get('name') in PROFILES.values()
                        for o in p['metadata'].get('ownerReferences', []))]
        ok = len(pods) == 1 and expected is not None
        if ok:
            pod = pods[0]
            ok = (any(o.get('uid') == ds.get('metadata', {}).get('uid') and o.get('controller')
                      for o in pod['metadata'].get('ownerReferences', []))
                  and not pod['metadata'].get('deletionTimestamp')
                  and any(c['type'] == 'Ready' and c['status'] == 'True' for c in pod.get('status', {}).get('conditions', []))
                  and pod['spec']['containers'] == ds['spec']['template']['spec']['containers'])
        checks['oneReadyPluginPerNode'] &= bool(ok)
        assignments.append({'node': name, 'nodeUID': node['metadata']['uid'], 'build': build,
                            'expectedProfile': expected, 'pods': [p['metadata']['name'] for p in pods], 'passed': bool(ok)})
    return {'passed': all(checks.values()), 'checks': checks, 'assignments': assignments,
            'scope': 'Test plugin placement and profile contract; GPU execution is a separate gate'}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--api-server', required=True);p.add_argument('--bastion', required=True)
    p.add_argument('--output', required=True, type=pathlib.Path);a = p.parse_args()
    cmd = ['docker', 'exec', a.bastion, 'kubectl', '--server='+a.api_server, '-n', 'cldt-windows-gpu']
    def get(kind):
        return json.loads(subprocess.check_output(cmd+['get', kind, '-o', 'json'], timeout=30))
    snapshot = {k: get(k) for k in ['nodes', 'daemonsets', 'pods']}
    result = evaluate(snapshot)
    result['nodeIdentitiesStable'] = {n['metadata']['name']: n['metadata']['uid'] for n in snapshot['nodes']['items']} == {n['metadata']['name']: n['metadata']['uid'] for n in get('nodes')['items']}
    result['passed'] &= result['nodeIdentitiesStable']
    result['observedAt'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    result['snapshot'] = snapshot
    with a.output.open('x') as f: json.dump(result, f, indent=2);f.write('\n')
    print(json.dumps({k: result[k] for k in ['passed', 'checks', 'assignments']}))
    if not result['passed']: raise SystemExit(1)


if __name__ == '__main__': main()
