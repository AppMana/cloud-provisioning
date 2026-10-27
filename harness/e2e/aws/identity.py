#!/usr/bin/env python3
"""Give CAPA this run's identity through the explicitly named bastion.

As an operator does: the source user's long-lived key in an
AWSClusterStaticIdentity, and an AWSClusterRoleIdentity through which CAPA
assumes the run's CAPA role and refreshes the session itself.
"""
import argparse
import base64
import json
import pathlib
import subprocess


def objects(state, key):
    source = state['runID'] + '-source'
    return [
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': source, 'namespace': 'capa-system'}, 'type': 'Opaque',
         'data': {k: base64.b64encode(key[k].encode()).decode() for k in ('AccessKeyID', 'SecretAccessKey')}},
        {'apiVersion': 'infrastructure.cluster.x-k8s.io/v1beta2', 'kind': 'AWSClusterStaticIdentity', 'metadata': {'name': source},
         'spec': {'secretRef': source, 'allowedNamespaces': {'list': ['cloud-provisioning']}}},
        {'apiVersion': 'infrastructure.cluster.x-k8s.io/v1beta2', 'kind': 'AWSClusterRoleIdentity', 'metadata': {'name': state['runID']},
         'spec': {'roleARN': state['capaRoleARN'], 'sessionName': state['runID'], 'durationSeconds': 3600,
                  'sourceIdentityRef': {'kind': 'AWSClusterStaticIdentity', 'name': source},
                  'allowedNamespaces': {'list': ['cloud-provisioning']}}},
    ]


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--work-dir', required=True)
    p.add_argument('--api-server', required=True)
    p.add_argument('--bastion', default='clab-cldt-bastion')
    a = p.parse_args()
    work = pathlib.Path(a.work_dir)
    state = json.loads((work / 'resources.json').read_text())
    if state.get('cleanedUp'):
        raise RuntimeError('run has been cleaned up')
    key = json.loads((work / 'capa-source-credentials.json').read_text())
    for o in objects(state, key):
        # Server-side, so the keys are not copied into a last-applied annotation.
        r = subprocess.run(['docker', 'exec', '-i', a.bastion, 'kubectl', '--server=' + a.api_server, 'apply', '--server-side',
                            '--field-manager=cldt-operator', '-f', '-'], input=json.dumps(o), capture_output=True, text=True)
        if r.returncode:
            raise RuntimeError('CAPA identity publication failed')
    print('Published the CAPA role identity', state['runID'])


if __name__ == '__main__':
    main()
