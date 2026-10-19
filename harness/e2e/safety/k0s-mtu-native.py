#!/usr/bin/env python3
"""Rehearse an owned k0s MTU change only in an isolated native Docker lab."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import time
import uuid

IMAGE = 'registry.k8s.io/e2e-test-images/agnhost:2.56@sha256:352a050380078cb2a1c246357a0dfa2fcf243ee416b92ff28b44a01d1b4b0294'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runner', required=True, help='isolated Docker-in-Docker runner; never a host socket')
    parser.add_argument('--guard', type=Path, required=True)
    parser.add_argument('--artifacts', type=Path, required=True, help='fresh private artifact directory')
    parser.add_argument('--candidate-mtu', type=int, default=1400)
    parser.add_argument('--repetitions', type=int, default=3)
    args = parser.parse_args()
    if not 1280 <= args.candidate_mtu <= 1450 or args.repetitions < 1:
        parser.error('require a dual-stack MTU and at least one repetition')
    args.artifacts.mkdir(mode=0o700, parents=True, exist_ok=False)
    events = args.artifacts / 'commands.jsonl'

    def run(argv, data=None, timeout=45):
        result = subprocess.run(argv, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        with events.open('a') as log:
            log.write(json.dumps({'monotonicSeconds': time.monotonic(), 'argv': argv, 'code': result.returncode,
                                  'stdout': result.stdout.decode(errors='replace'),
                                  'stderr': result.stderr.decode(errors='replace')}) + '\n')
        if result.returncode:
            raise RuntimeError(f'command failed ({result.returncode}): {argv}: {result.stderr.decode(errors="replace")}')
        return result.stdout

    prefix = ['docker', 'exec', '-i', args.runner, 'docker']
    outer = run(['docker', 'info', '--format', '{{.ID}}']).strip()
    inner = run(prefix + ['info', '--format', '{{.ID}}']).strip()
    if outer == inner or not inner:
        raise RuntimeError('refusing shared host Docker daemon')
    inspection = json.loads(run(['docker', 'inspect', args.runner]))[0]
    if inspection['HostConfig']['NetworkMode'] == 'host' or inspection['HostConfig']['PidMode'] == 'host' or any(m['Destination'] == '/var/run/docker.sock' for m in inspection['Mounts']):
        raise RuntimeError('runner must isolate networking and the Docker socket')

    def guest(node, command, timeout=45):
        return run(prefix + ['exec', '-i', 'clab-cldt-' + node, '/cldt-guest', 'exec', str(timeout) + 's', '0', 'sh', '-ec', command], timeout=timeout + 10)

    def put(node, path, data, mode='0600'):
        run(prefix + ['exec', '-i', 'clab-cldt-' + node, '/cldt-guest', 'put', '30s', mode, path], data)

    def kube(*argv, data=None, timeout=45):
        return run(prefix + ['exec', '-i', 'clab-cldt-bastion', 'kubectl', '--request-timeout=15s', *argv], data, timeout)

    # Fixed disposable topology identities prevent accidentally targeting another cluster.
    nodes = json.loads(kube('get', 'nodes', '-o', 'json'))['items']
    if {n['metadata']['name'] for n in nodes} != {'cp', 'w1', 'w2'}:
        raise RuntimeError('require the disposable three-node native site')
    for node in nodes:
        expected = {'cp': '10.101.0.1', 'w1': '10.101.0.2', 'w2': '10.101.0.3'}[node['metadata']['name']]
        if not any(a['address'] == expected for a in node['status']['addresses']):
            raise RuntimeError('unexpected site addressing')
    original = guest('cp', 'cat /etc/k0s/k0s.yaml')
    (args.artifacts / 'original-k0s.yaml').write_bytes(original)
    original_hash = hashlib.sha256(original).hexdigest()
    original_mtu = int(guest('w1', "python3 -c 'import json; print(json.load(open(\"/etc/cni/net.d/10-calico.conflist\"))[\"plugins\"][0][\"mtu\"])'"))
    if original_mtu == args.candidate_mtu:
        raise RuntimeError('candidate must differ from the current MTU')
    transaction = 'mtu-' + uuid.uuid4().hex[:12]
    namespace = 'guard-' + transaction
    root = '/var/tmp/' + transaction
    guest('cp', f'mkdir -m 700 {root}')
    put('cp', root + '/network-guard', args.guard.read_bytes(), '0700')
    kube('create', 'namespace', namespace)
    completed = False
    samplers = []
    pod_samplers = []
    results = []

    def pod(name, node):
        manifest = {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': name, 'namespace': namespace},
                    'spec': {'nodeName': node, 'containers': [{'name': 'probe', 'image': IMAGE,
                     'imagePullPolicy': 'Never', 'command': ['sh', '-c', 'exec sleep 1800']}],
                     'terminationGracePeriodSeconds': 0}}
        kube('create', '-f', '-', data=json.dumps(manifest).encode())
        kube('-n', namespace, 'wait', '--for=condition=Ready', 'pod/' + name, '--timeout=60s', timeout=70)
        return json.loads(kube('-n', namespace, 'get', 'pod', name, '-o', 'json'))

    def mtu(name):
        return int(kube('-n', namespace, 'exec', name, '--', 'cat', '/sys/class/net/eth0/mtu'))

    try:
        pod('stable-w1', 'w1')
        remote = pod('stable-w2', 'w2')
        if mtu('stable-w1') != original_mtu or mtu('stable-w2') != original_mtu:
            raise RuntimeError('baseline pod MTU mismatch')
        # Independent host processes survive control-plane/API and CNI pod restarts.
        for number, address in enumerate(['10.101.0.1', 'fd8f:cf26:522a::1'] + [p['ip'] for p in remote['status']['podIPs']]):
            unit = transaction + '-probe-' + str(number)
            logfile = '/var/tmp/' + unit + '.log'
            guest('w1', f"systemd-run --quiet --unit {unit} /bin/sh -c 'exec ping -n -i 0.2 -w 1200 {address} > {logfile} 2>&1'")
            samplers.append((unit, logfile, address))
        for number, endpoint in enumerate(remote['status']['podIPs']):
            address = endpoint['ip']
            logfile = '/tmp/guard-ping-' + str(number) + '.log'
            pid = int(kube('-n', namespace, 'exec', 'stable-w1', '--', 'sh', '-c',
                           f'nohup ping -n -i 0.2 -w 1200 {address} > {logfile} 2>&1 < /dev/null & echo $!'))
            pod_samplers.append((pid, logfile, address))
        for repetition in range(1, args.repetitions + 1):
            location = root + '/run-' + str(repetition)
            guest('cp', f'mkdir -m 700 {location}')
            # k0s inlines MTU in the DaemonSet's CNI configuration. Changing the
            # owned MTU must trigger the normal rollout without a test-only nudge.
            candidate_builder = '''import sys,yaml
p=yaml.safe_load(open('/etc/k0s/k0s.yaml'))
c=p['spec']['network']['calico']; c['mtu']=int(sys.argv[2])
open(sys.argv[1],'w').write(yaml.safe_dump(p))
'''
            put('cp', location + '/candidate.py', candidate_builder.encode())
            guest('cp', f'python3 {location}/candidate.py {location}/candidate {args.candidate_mtu}; chmod 600 {location}/candidate')
            candidate = guest('cp', f'cat {location}/candidate')
            command = b'#!/bin/sh\nset -eu\nsystemctl restart k0scontroller\n'
            put('cp', location + '/reload', command, '0700')
            plan = {'apply': location + '/reload', 'restore': location + '/reload',
                    'files': [{'path': '/etc/k0s/k0s.yaml', 'candidatePath': location + '/candidate',
                               'candidateSHA256': hashlib.sha256(candidate).hexdigest()}]}
            put('cp', location + '/plan.json', json.dumps(plan).encode())
            guest('cp', f'{root}/network-guard -state {location}/state -plan {location}/plan.json -within 120s prepare')
            unit = transaction + '-' + str(repetition)
            began = time.monotonic()
            guest('cp', f"systemd-run --quiet --unit {unit} --property Restart=on-failure --property RestartSec=1s --property KillMode=control-group --property 'RestrictAddressFamilies=AF_UNIX AF_NETLINK' {root}/network-guard -state {location}/state watch")
            # Poll local installed CNI files without interpreting transient API
            # availability as a data-plane connectivity result.
            candidate_seen = False
            while time.monotonic() - began < 95:
                values = [int(guest(n, "python3 -c 'import json; print(json.load(open(\"/etc/cni/net.d/10-calico.conflist\"))[\"plugins\"][0][\"mtu\"])'")) for n in ('cp', 'w1', 'w2')]
                if values == [args.candidate_mtu] * 3:
                    candidate_seen = True
                    break
                time.sleep(2)
            if not candidate_seen:
                raise RuntimeError('candidate CNI installation did not converge before rollback')
            kube('-n', 'kube-system', 'rollout', 'status', 'daemonset/calico-node', '--timeout=40s', timeout=50)
            candidate_name = 'candidate-' + str(repetition)
            pod(candidate_name, 'w2')
            if mtu(candidate_name) != args.candidate_mtu:
                raise RuntimeError('candidate did not change actual new pod MTU')
            while time.monotonic() - began < 290:
                state = json.loads(guest('cp', f'cat {location}/state/state.json'))
                values = [int(guest(n, "python3 -c 'import json; print(json.load(open(\"/etc/cni/net.d/10-calico.conflist\"))[\"plugins\"][0][\"mtu\"])'")) for n in ('cp', 'w1', 'w2')]
                if state['outcome'] == 'rolled-back' and values == [original_mtu] * 3:
                    break
                time.sleep(2)
            else:
                raise RuntimeError('rollback did not restore installed CNI within five minutes')
            if state['applyFailures'] or state['restoreFailures']:
                raise RuntimeError('eventual restoration cannot hide an earlier failure')
            kube('-n', 'kube-system', 'rollout', 'status', 'daemonset/calico-node', '--timeout=40s', timeout=50)
            restored_name = 'restored-' + str(repetition)
            pod(restored_name, 'w2')
            if mtu(restored_name) != original_mtu:
                raise RuntimeError('rollback did not restore actual new pod MTU')
            if guest('cp', 'cat /etc/k0s/k0s.yaml') != original:
                raise RuntimeError('original configuration bytes were not restored')
            results.append({'repetition': repetition, 'candidateMTU': args.candidate_mtu, 'restoredMTU': original_mtu,
                            'recoverySeconds': time.monotonic() - began, 'applyFailures': state['applyFailures'],
                            'restoreFailures': state['restoreFailures']})
            print(json.dumps(results[-1]), flush=True)
        completed = True
    finally:
        measurements = []
        for pid, logfile, address in pod_samplers:
            kube('-n', namespace, 'exec', 'stable-w1', '--', 'sh', '-c', f'kill -INT {pid}; sleep 1')
            output = kube('-n', namespace, 'exec', 'stable-w1', '--', 'cat', logfile).decode()
            (args.artifacts / Path(logfile).name).write_text(output)
            match = re.search(r'(\d+) packets transmitted, (\d+) (?:packets )?received', output)
            measurements.append({'source': 'stable-w1-pod', 'address': address, 'sent': int(match[1]) if match else 0,
                                 'received': int(match[2]) if match else 0})
        for unit, logfile, address in samplers:
            guest('w1', f'systemctl kill --signal=SIGINT {unit}; sleep 1')
            output = guest('w1', f'cat {logfile}').decode()
            (args.artifacts / (unit + '.log')).write_text(output)
            match = re.search(r'(\d+) packets transmitted, (\d+) received', output)
            measurements.append({'source': 'w1-host', 'address': address, 'sent': int(match[1]) if match else 0,
                                 'received': int(match[2]) if match else 0})
        passed = completed and all(m['sent'] >= 100 and m['sent'] == m['received'] for m in measurements)
        (args.artifacts / 'result.json').write_text(json.dumps({'passed': passed, 'originalSHA256': original_hash,
                        'guardSHA256': hashlib.sha256(args.guard.read_bytes()).hexdigest(),
                        'repetitions': results, 'probes': measurements, 'namespace': namespace, 'stateDirectory': root}, indent=2))
        # Leave failed runs and their autonomous guards intact for diagnosis.
        if passed:
            kube('delete', 'namespace', namespace, '--wait=false')
        if not passed:
            raise RuntimeError('migration qualification failed; retained artifacts and guard state')


if __name__ == '__main__':
    main()
