#!/usr/bin/env python3
"""Renew only the rotating observer Secret in an explicitly bound isolated lab.

Requires setup credentials able to request STS federation sessions. Never
changes IAM, CNI, routing, workloads, or controller configuration. All command
errors are redacted; private receipts contain identities and expiry, not keys.
"""
import argparse
import base64
import datetime as dt
import json
import os
from pathlib import Path
import subprocess
import time


OWNER = 'cloud-provisioning-test'
SECRET = 'native-pod-observer-file'
NAMESPACE = 'cloud-provisioning'


def command(argv, body=None):
    result = subprocess.run(argv, input=body, capture_output=True, text=True, timeout=90)
    if result.returncode:
        raise RuntimeError('external command failed; mutation outcome may be ambiguous; inspect target before retry')
    return result.stdout


def policy(region):
    return {'Version': '2012-10-17', 'Statement': [{'Effect': 'Allow',
        'Action': 'ec2:DescribeNetworkInterfaces', 'Resource': '*',
        'Condition': {'StringEquals': {'aws:RequestedRegion': region}}}]}


def renewal(state, runner, cluster_uid, call=command):
    if state.get('cleanedUp') or not state.get('runID', '').startswith('cldt-') or not cluster_uid:
        raise ValueError('active isolated campaign and explicit cluster UID required')
    inspection = json.loads(call(['docker', 'inspect', runner]))[0]
    config = inspection['HostConfig']
    if config['NetworkMode'] == 'host' or config['PidMode'] == 'host' or any(
            m['Destination'] == '/var/run/docker.sock' for m in inspection['Mounts']):
        raise ValueError('runner must isolate networking and the Docker socket')
    outer = call(['docker', 'info', '--format', '{{.ID}}']).strip()
    inner = call(['docker', 'exec', runner, 'docker', 'info', '--format', '{{.ID}}']).strip()
    if not inner or inner == outer:
        raise ValueError('shared Docker daemon rejected')
    prefix = ['docker', 'exec', '-i', runner, 'docker', 'exec', '-i', 'clab-cldt-bastion',
              'kubectl', '--server=https://10.101.0.1:6443', '--request-timeout=30s']

    def kube(*args, body=None):
        raw = call(prefix + list(args), body)
        return json.loads(raw) if raw.strip() else None

    def identity():
        if kube('get', 'namespace', 'kube-system', '-o', 'json')['metadata']['uid'] != cluster_uid:
            raise ValueError('cluster identity mismatch')

    identity()
    existing = kube('-n', NAMESPACE, 'get', 'secret', SECRET, '--ignore-not-found', '-o', 'json')
    if existing and (existing['metadata'].get('labels', {}).get(OWNER) != state['runID'] or
                     existing.get('type') != 'Opaque'):
        raise ValueError('existing Secret ownership or type mismatch')
    aws = ['aws', '--region', state['region']]
    account = json.loads(call(aws + ['sts', 'get-caller-identity', '--output', 'json']))['Account']
    if account != state['account']:
        raise ValueError('AWS account mismatch')
    response = json.loads(call(aws + ['sts', 'get-federation-token', '--name', 'cldt-pod-observer',
        '--duration-seconds', '3600', '--policy', json.dumps(policy(state['region'])), '--output', 'json']))
    credentials = response['Credentials']
    expiry = dt.datetime.fromisoformat(credentials['Expiration'].replace('Z', '+00:00'))
    now = dt.datetime.now(dt.timezone.utc)
    if expiry.tzinfo is None or not 1200 <= (expiry - now).total_seconds() <= 3700:
        raise ValueError('unexpected credential expiration')
    if any(not isinstance(credentials.get(key), str) or not credentials[key].strip()
           for key in ['AccessKeyId', 'SecretAccessKey', 'SessionToken']):
        raise ValueError('incomplete temporary credentials')
    # Publish only the provider's defined schema; no STS response metadata.
    document = {key: credentials[key] for key in ['AccessKeyId', 'SecretAccessKey', 'SessionToken', 'Expiration']}
    encoded = base64.b64encode(json.dumps(document).encode()).decode()
    if existing:
        obj = existing
        obj.setdefault('data', {})['credentials.json'] = encoded
        operation = 'replace'  # resourceVersion prevents overwriting concurrent changes.
    else:
        obj = {'apiVersion': 'v1', 'kind': 'Secret', 'type': 'Opaque',
               'metadata': {'name': SECRET, 'namespace': NAMESPACE, 'labels': {OWNER: state['runID']}},
               'data': {'credentials.json': encoded}}
        operation = 'create'
    identity()
    # No automatic retry: a timeout may mean the write committed.
    published = kube('-n', NAMESPACE, operation, '-f', '-', '-o', 'json', body=json.dumps(obj))
    if published.get('data', {}).get('credentials.json') != encoded:
        raise RuntimeError('published credential content differs')
    metadata = published['metadata']
    if (metadata['name'] != SECRET or metadata['namespace'] != NAMESPACE or
            metadata.get('labels', {}).get(OWNER) != state['runID'] or
            (existing and metadata['uid'] != existing['metadata']['uid'])):
        raise RuntimeError('published Secret identity differs')
    return {'recordedAt': now.isoformat(), 'expiresAt': expiry.isoformat(), 'clusterUID': cluster_uid,
            'secretUID': metadata['uid'], 'resourceVersion': metadata['resourceVersion'],
            'operation': operation, 'runID': state['runID']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--runner', required=True)
    parser.add_argument('--cluster-uid', required=True, help='observed kube-system namespace UID')
    parser.add_argument('--receipts', type=Path, required=True, help='new private output directory')
    parser.add_argument('--duration', type=int, default=0, help='seconds to maintain credentials; 0 renews once, maximum 172800')
    args = parser.parse_args()
    if not 0 <= args.duration <= 172800:
        parser.error('duration must be 0..172800 seconds')
    os.umask(0o077)
    args.receipts.mkdir(mode=0o700, parents=True, exist_ok=False)
    deadline = time.monotonic() + args.duration
    index = 0
    try:
        while True:
            # Re-read cleanup state before every renewal; never rewrite resources.json.
            state = json.loads((args.work_dir / 'resources.json').read_text())
            with (args.receipts / f'{index:04}-intent.json').open('x') as out:
                json.dump({'at': dt.datetime.now(dt.timezone.utc).isoformat(),
                           'runner': args.runner, 'clusterUID': args.cluster_uid}, out)
            receipt = renewal(state, args.runner, args.cluster_uid)
            with (args.receipts / f'{index:04}-complete.json').open('x') as out:
                json.dump(receipt, out, indent=2)
            print(json.dumps(receipt), flush=True)
            index += 1
            if not args.duration:
                break
            expires = dt.datetime.fromisoformat(receipt['expiresAt'])
            delay = max(1, (expires - dt.datetime.now(dt.timezone.utc)).total_seconds() - 600)
            wake = min(deadline, time.monotonic() + delay)
            while time.monotonic() < wake:
                time.sleep(max(0, min(30, wake - time.monotonic())))
            if time.monotonic() >= deadline:
                break
    except Exception:
        # Exceptions from JSON, subprocess or SDK responses can contain secrets.
        with (args.receipts / 'failed.json').open('x') as out:
            json.dump({'at': dt.datetime.now(dt.timezone.utc).isoformat(), 'attempt': index,
                       'error': 'renewal failed; inspect target state before retry'}, out)
        raise SystemExit('Observer credential renewal failed; private receipt retained; no automatic retry') from None
    with (args.receipts / 'terminal.json').open('x') as out:
        json.dump({'completed': True, 'renewals': index, 'durationSeconds': args.duration}, out)


if __name__ == '__main__':
    main()
