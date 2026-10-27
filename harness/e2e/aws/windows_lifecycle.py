#!/usr/bin/env python3
"""Run a Windows CAPA add/gateway/traffic/remove trial against an existing VM site.

Requires a native out-of-band survivor Session started before the trial. Failed stages retain the
original resources and evidence for diagnosis; rerunning never resumes a claim.
"""
import argparse
import copy
import datetime
import json
import pathlib
import re
import subprocess
import sys
import time

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / 'observe'))
from removal import evaluate, preflight


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def validate_config(config):
    required = ['bastion', 'apiServer', 'name', 'probeNamespace', 'sourcePod',
                'windowsVersion', 'workDirectory', 'siteDirectory', 'awsnode',
                'awsremove', 'gateway', 'observerDirectory']
    if any(not isinstance(config.get(key), str) or not config[key].strip() for key in required):
        raise ValueError('Complete nonempty lifecycle configuration required before provisioning')
    if config['windowsVersion'] not in ['2022', '2025']:
        raise ValueError('Windows version must be 2022 or 2025')
    if not config.get('observerExecutors'):
        raise ValueError('Native observer requires trusted OOB executor mapping path')
    targets = config.get('survivorTargets')
    if (not isinstance(targets, list) or len(targets) < 2 or
            any(not isinstance(t, str) or t.count('=') != 1 or not all(t.split('=')) for t in targets) or
            len({t.split('=')[0] for t in targets}) != len(targets)):
        raise ValueError('Distinct survivor Pod=NodeUID targets required')


def probe_objects(source, service, node, name, namespace, version):
    build = {'2022': '20348', '2025': '26100'}[version]
    if (node['metadata']['labels'].get('kubernetes.io/os') != 'windows' or
            str(node['metadata']['labels'].get('node.kubernetes.io/windows-build', '')).split('.')[-1] != build):
        raise ValueError('Target Node does not match requested Windows build')
    containers = source['spec']['containers']
    if len(containers) != 1 or not re.fullmatch(r'.+@sha256:[0-9a-f]{64}', containers[0]['image']):
        raise ValueError('Probe source must have one digest-pinned container')
    container = copy.deepcopy(containers[0])
    container.pop('volumeMounts', None)
    spec = {'containers': [container], 'nodeSelector': {
        'kubernetes.io/os': 'windows', 'kubernetes.io/hostname': node['metadata']['name']},
        'os': {'name': 'windows'}, 'tolerations': copy.deepcopy(source['spec'].get('tolerations', [])),
        'restartPolicy': 'Never'}
    return [dict(apiVersion='v1', kind='Pod', metadata=dict(name=name, namespace=namespace, labels={'app': name}), spec=spec),
            dict(apiVersion='v1', kind='Service', metadata=dict(name=name, namespace=namespace),
                 spec=dict(selector={'app': name}, ports=copy.deepcopy(service['spec']['ports'])))]


class Trial:
    def __init__(self, config, output):
        self.c = config
        self.output = pathlib.Path(output)
        self.k = ['docker', 'exec', '-i', config['bastion'], 'kubectl', '--server=' + config['apiServer']]
        self.root = pathlib.Path(__file__).resolve().parents[1]

    def save(self, name, value):
        with (self.output / name).open('x') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')

    def get(self, kind, name=None, namespace='cloud-provisioning'):
        args = self.k + (['-n', namespace] if namespace else []) + ['get', kind]
        args += [name, '--ignore-not-found'] if name else []
        p = subprocess.run(args + ['-o', 'json'], capture_output=True, text=True, check=True, timeout=30)
        return json.loads(p.stdout) if p.stdout.strip() else None

    def snapshot(self):
        return {kind: self.get(kind, namespace='' if kind == 'nodes' else 'cloud-provisioning')
                for kind in ['nodes', 'machines', 'configmaps']}

    def run(self, args, label):
        with (self.output / (label + '.stdout')).open('x') as out, (self.output / (label + '.stderr')).open('x') as err:
            p = subprocess.Popen(args, stdout=out, stderr=err)
            self.save(label + '-process.json', dict(pid=p.pid, startedAt=now()))
            code = p.wait()
        self.save(label + '-exit.json', dict(exitCode=code, finishedAt=now()))
        if code:
            raise RuntimeError(label + ' failed; inspect the original operation and retained resources')

    def helper(self, file, *args):
        return [sys.executable, str(self.root / file), *args]

    def native_observer(self):
        from survivor_oob import OutOfBand
        from survivor_native import Session
        c=self.c
        kube=OutOfBand(c['apiServer'],c['bastion'],c['probeNamespace'],
            json.loads(pathlib.Path(c['observerExecutors']).read_text()))
        return Session(kube,c['observerDirectory'])

    def survivor_ready(self, label):
        return self.native_observer().live_ready(self.c['survivorTargets'],label)

    def finish_survivors(self, operation):
        session=self.native_observer()
        def dump(source,args):
            return subprocess.run(session.kube.command+['exec','-i',source['pod'],
                '-c',source['container'],'--']+args,capture_output=True,check=True,timeout=120).stdout
        report=session.finish(operation['startedAt'],operation['finishedAt'],dump)
        self.save('survivor-result.json',report)
        if not report['ok']:
            raise RuntimeError('Lifecycle finished but native survivor evidence failed; retain original logs')
        return report

    def execute(self):
        c = self.c
        validate_config(c)
        name, namespace = c['name'], c['probeNamespace']
        podname = 'net-' + name
        self.output.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.save('configuration.json', c)
        if not self.survivor_ready('before-addition'):
            raise RuntimeError('Survivor observer is not actively passing; no claim created')
        for kind, resource, ns in [('provisionednodeclaim', name, 'cloud-provisioning'),
                                   ('machine', name, 'cloud-provisioning'),
                                   ('awsmachinetemplate', name, 'cloud-provisioning'),
                                   ('configmap', 'gateway-' + name, 'cloud-provisioning'),
                                   ('pod', podname, namespace), ('service', podname, namespace)]:
            if self.get(kind, resource, ns):
                raise RuntimeError('Resource already exists: ' + kind + '/' + resource)
        source = self.get('pod', c['sourcePod'], namespace)
        service = self.get('service', c['sourcePod'], namespace)
        if not source or not service:
            raise RuntimeError('Existing ordinary probe Pod and Service required')
        # Validate the recipe before provisioning; the target build is checked
        # again using the actual new Node after association.
        source_node = self.get('node', source['spec']['nodeName'], '')
        probe_objects(source, service, source_node, podname, namespace, c['windowsVersion'])
        self.save('baseline.json', self.snapshot())
        operation = dict(claim=name, startedAt=now())
        self.save('operation-start.json', operation)
        common = ['--work-dir', c['workDirectory'], '--api-server', c['apiServer'], '--bastion', c['bastion']]
        # The claim names its gateway; the product derives the attachment.
        self.run(self.helper('aws/claim.py', *common, '--name', name, '--windows-version', c['windowsVersion'], '--cni', 'site',
                             '--gateway', c['gateway']), 'create')
        for _ in range(120):
            machine = self.get('machine', name)
            node_name = (machine or {}).get('status', {}).get('nodeRef', {}).get('name')
            node = self.get('node', node_name, '') if node_name else None
            if node and any(x['type'] == 'Ready' and x['status'] == 'True' for x in node.get('status', {}).get('conditions', [])):
                break
            time.sleep(10)
        else:
            raise RuntimeError('Original worker not Ready within observation; inspect it without recreating')
        binding = dict(claim=name, machineUID=machine['metadata']['uid'], node=node_name,
                       nodeUID=node['metadata']['uid'], providerID=machine['spec']['providerID'])
        self.save('binding.json', binding)
        for attempt in range(60):
            snapshot = self.snapshot()
            guard = preflight(snapshot, name, binding['machineUID'])
            self.save(f'attachment-{attempt:02d}.json', dict(snapshot=snapshot, guard=guard))
            if guard['passed']:
                break
            time.sleep(10)
        else:
            raise RuntimeError('Gateway attachment not qualified; no workload created')
        self.run(self.helper('aws/windows_join.py', *common, '--name', name, '--windows-version', c['windowsVersion'],
                             '--awsnode', c['awsnode'], '--require-bootstrap-completion', '--require-peer-delivery', '--require-gateway',
                             '--output', str(self.output / 'join.json')), 'join')
        # Do not create a workload on a same-name replacement Node.
        current = self.get('node', node_name, '')
        if not current or current['metadata']['uid'] != binding['nodeUID']:
            raise RuntimeError('Worker identity changed before workload creation')
        for index, obj in enumerate(probe_objects(source, service, current, podname, namespace, c['windowsVersion'])):
            self.save(f'probe-intent-{index}.json', obj)
            p = subprocess.run(self.k + ['create', '-f', '-'], input=json.dumps(obj), capture_output=True, text=True)
            self.save(f'probe-create-{index}.json', dict(exitCode=p.returncode))
            if p.returncode:
                raise RuntimeError('Probe submission failed; inspect original object, do not repeat creation')
        self.run(self.k + ['-n', namespace, 'wait', 'pod/' + podname, '--for=condition=Ready', '--timeout=10m'], 'probe-ready')
        args = self.helper('observe/pod_matrix.py', '--api-server', c['apiServer'], '--bastion', c['bastion'],
                           '--namespace', namespace, '--tries', '20', '--udp-payload-bytes', '64', '--output', str(self.output / 'matrix.json'))
        for target in c['survivorTargets'] + [podname + '=' + binding['nodeUID']]:
            args += ['--target', target]
        self.run(args, 'matrix')
        operation['additionFinishedAt'] = now()
        before = self.snapshot()
        guard = preflight(before, name, binding['machineUID'])
        self.save('before-removal.json', before)
        self.save('removal-preflight.json', guard)
        if not guard['passed'] or not self.survivor_ready('before-removal'):
            raise RuntimeError('Removal preflight failed; retain original worker for diagnosis')
        operation['removalStartedAt'] = now()
        self.save('operation-removing.json', operation)
        self.run([c['awsremove'], '-work-dir', c['workDirectory'], '-site-work-dir', c['siteDirectory'],
                  '-claim', name, '-report', str(self.output / 'removal.json')], 'removal')
        receipt = json.loads((self.output / 'removal.json').read_text())
        for attempt in range(60):
            after = self.snapshot()
            result = evaluate(before, after, receipt)
            self.save(f'convergence-{attempt:02d}.json', dict(snapshot=after, result=result))
            if result['passed']:
                break
            time.sleep(10)
        else:
            raise RuntimeError('Original removal has not converged; do not repeat deletion')
        operation.update(finishedAt=now(), lifecyclePassed=True, survivorPassed=False,
                         survivorStatus='incomplete', passed=False)
        self.save('operation-window.json', operation)
        try:
            survivor = self.finish_survivors(operation)
            operation['survivorStatus'] = 'complete'
            operation['survivorPassed'] = bool(survivor['ok'])
            operation['passed'] = operation['lifecyclePassed'] and operation['survivorPassed']
        except Exception:
            operation['survivorStatus'] = 'failed-or-incomplete'
            raise
        finally:
            self.save('operation.json', operation)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', type=pathlib.Path, required=True)
    p.add_argument('--output', type=pathlib.Path, required=True)
    args = p.parse_args()
    Trial(json.loads(args.config.read_text()), args.output.resolve()).execute()


if __name__ == '__main__':
    main()
