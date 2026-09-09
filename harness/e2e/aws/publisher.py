#!/usr/bin/env python3
"""Publish a test Windows image to run-owned private ECR.

Source the setup identity. Authentication stays in a temporary Docker config;
the isolated cluster receives an expiring imagePullSecret, never setup keys.
"""
import argparse
import base64
import json
import os
import pathlib
import subprocess
import tempfile
from workload_platform import layout_platform

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--work-dir', required=True, type=pathlib.Path)
artifact = p.add_mutually_exclusive_group(required=True)
artifact.add_argument('--layout', type=pathlib.Path)
artifact.add_argument('--archive', type=pathlib.Path, help='Docker image tar archive')
artifact.add_argument('--renew-pull-only', action='store_true', help='renew access to the recorded immutable image without pushing it again')
COMPONENTS = {
    'windows-publisher': ('publisher', 'cloud-provisioning', 'publisher-pull'),
    'calico-node-windows': ('calicoWindows', 'kube-system', 'calico-pull'),
    'windows-gpu-probe': ('windowsGPUProbe', 'cldt-windows-gpu', 'gpu-probe-pull'),
    'windows-gpu-device-plugin': ('windowsGPUPlugin', 'cldt-windows-gpu', 'gpu-plugin-pull'),
}
p.add_argument('--component', choices=COMPONENTS, default='windows-publisher')
p.add_argument('--windows-version', choices=['2022','2025'], help='record a build-checked GPU workload separately for this Windows release')
p.add_argument('--api-server', required=True)
p.add_argument('--bastion', default='clab-cldt-bastion')
a = p.parse_args()
if a.windows_version and (a.component!='windows-gpu-probe' or a.archive):
    raise RuntimeError('Windows release selection requires a GPU probe OCI layout or recorded pull renewal')
path = a.work_dir/'resources.json'
s = json.loads(path.read_text())
if s.get('cleanedUp') or not s['runID'].startswith('cldt-'):
    p.error('require an active cldt run')
state_key, namespace, secret_suffix = COMPONENTS[a.component]
image_state = s if not a.windows_version else s.setdefault(state_key+'ByWindowsVersion', {}).setdefault(a.windows_version, {})
def key(suffix): return state_key+suffix if not a.windows_version else suffix[0].lower()+suffix[1:]
platform = layout_platform(a.layout, a.windows_version) if a.windows_version and a.layout else None
if a.renew_pull_only:
    recorded = image_state.get(key('Image'), '')
    if '@sha256:' not in recorded: raise RuntimeError('recorded immutable image required for pull renewal')
    digest = recorded.rsplit('@', 1)[1]
elif a.layout:
    index = json.loads((a.layout/'index.json').read_text())
    if len(index['manifests']) != 1: raise RuntimeError('require exactly one image')
    digest = index['manifests'][0]['digest']
else:
    digest = subprocess.run(['crane', 'digest', '--tarball', str(a.archive)], capture_output=True, text=True, check=True).stdout.strip()

def aws(service, operation, inputs, missing=False, env=None):
    r = subprocess.run(['aws', '--region', s['region'], service, operation,
                        '--cli-input-json', json.dumps(inputs), '--output', 'json'], capture_output=True, text=True, env=env)
    if r.returncode:
        if missing and 'RepositoryNotFoundException' in r.stderr: return None
        raise RuntimeError(f'{service}/{operation} failed')
    return json.loads(r.stdout)

if aws('sts', 'get-caller-identity', {})['Account'] != s['account']:
    raise RuntimeError('AWS account mismatch')
name = s['runID']+'/'+a.component
existing = aws('ecr', 'describe-repositories', {'repositoryNames': [name]}, missing=True)
if existing:
    repo = existing['repositories'][0]
    tags = aws('ecr', 'list-tags-for-resource', {'resourceArn': repo['repositoryArn']})['tags']
    if {t['Key']: t['Value'] for t in tags}.get('cloud-provisioning-test') != s['runID']:
        raise RuntimeError('ECR repository ownership mismatch')
else:
    if a.renew_pull_only: raise RuntimeError('cannot renew pull access for a missing repository')
    repo = aws('ecr', 'create-repository', {'repositoryName': name, 'imageTagMutability': 'IMMUTABLE',
                'tags': [{'Key': 'cloud-provisioning-test', 'Value': s['runID']}]})['repository']
if a.renew_pull_only and recorded != repo['repositoryUri']+'@'+digest:
    raise RuntimeError('recorded image is outside the owned repository')
image_state[key('Repository')] = {'name': name, 'arn': repo['repositoryArn'], 'uri': repo['repositoryUri']}
# Repository and pull Secret ownership remain component-wide for cleanup,
# including runs whose first publication uses a Windows-version record.
s[state_key+'Repository'] = image_state[key('Repository')]
path.write_text(json.dumps(s, indent=2)+'\n')
# Digest-derived immutable tag makes retries safe and preserves previous builds.
ref = repo['repositoryUri']+':'+digest.split(':')[1]
registry = repo['repositoryUri'].split('/')[0]
if a.renew_pull_only:
    ref = recorded
else:
    auth = aws('ecr', 'get-authorization-token', {})['authorizationData'][0]
    config = {'auths': {registry: {'auth': auth['authorizationToken']}}}
    with tempfile.TemporaryDirectory(prefix='cldt-ecr-') as directory:
        cfg = pathlib.Path(directory)/'config.json'
        cfg.write_text(json.dumps(config)); cfg.chmod(0o600)
        env = dict(os.environ, DOCKER_CONFIG=directory)
        push = subprocess.run(['crane', 'push', str(a.layout or a.archive), ref], capture_output=True, text=True, env=env)
        if push.returncode: raise RuntimeError('publisher push failed')
        observed = subprocess.run(['crane', 'digest', ref], capture_output=True, text=True, env=env, check=True).stdout.strip()
        if observed != digest: raise RuntimeError('publisher registry digest differs from local OCI manifest')
# ECR tokens inherit their principal's rights. Never put the setup publisher's
# token in Kubernetes: obtain a separate session restricted to this repository.
pull_policy = {'Version': '2012-10-17', 'Statement': [
    {'Effect': 'Allow', 'Action': 'ecr:GetAuthorizationToken', 'Resource': '*'},
    {'Effect': 'Allow', 'Action': ['ecr:BatchGetImage', 'ecr:GetDownloadUrlForLayer',
                                 'ecr:BatchCheckLayerAvailability'], 'Resource': repo['repositoryArn']}]}
pull_session = aws('sts', 'get-federation-token', {'Name': 'cldt-publisher-pull', 'DurationSeconds': 3600,
                   'Policy': json.dumps(pull_policy, separators=(',', ':'))})['Credentials']
pull_env = dict(os.environ, AWS_ACCESS_KEY_ID=pull_session['AccessKeyId'],
                AWS_SECRET_ACCESS_KEY=pull_session['SecretAccessKey'], AWS_SESSION_TOKEN=pull_session['SessionToken'])
pull_env.pop('AWS_SECURITY_TOKEN', None)
pull_auth = aws('ecr', 'get-authorization-token', {}, env=pull_env)['authorizationData'][0]
config = {'auths': {registry: {'auth': pull_auth['authorizationToken']}}}
with tempfile.TemporaryDirectory(prefix='cldt-ecr-pull-') as directory:
    cfg = pathlib.Path(directory)/'config.json'
    cfg.write_text(json.dumps(config)); cfg.chmod(0o600)
    probe = subprocess.run(['crane', 'digest', ref], capture_output=True, text=True,
                           env=dict(os.environ, DOCKER_CONFIG=directory))
    if probe.returncode or probe.stdout.strip() != digest:
        raise RuntimeError('restricted publisher pull credential failed registry readback')
secret_name = s['runID']+'-'+secret_suffix
secret = {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': secret_name, 'namespace': namespace},
          'type': 'kubernetes.io/dockerconfigjson',
          'data': {'.dockerconfigjson': base64.b64encode(json.dumps(config).encode()).decode()}}
r = subprocess.run(['docker', 'exec', '-i', a.bastion, 'kubectl', '--server='+a.api_server, 'apply', '-f', '-'],
                   input=json.dumps(secret), capture_output=True, text=True)
if r.returncode: raise RuntimeError('isolated publisher pull-secret publication failed')
image_state[key('Image')] = repo['repositoryUri']+'@'+digest
image_state[key('PullSecret')] = secret_name
image_state[key('PullSessionExpiresAt')] = pull_session['Expiration']
s[state_key+'PullSecret'] = secret_name
s[state_key+'PullSessionExpiresAt'] = pull_session['Expiration']
for version_record in s.get(state_key+'ByWindowsVersion', {}).values():
    if version_record.get('image', '').startswith(repo['repositoryUri']+'@sha256:'):
        version_record['pullSecret'] = secret_name
        version_record['pullSessionExpiresAt'] = pull_session['Expiration']
if platform: image_state['platform'] = platform
path.write_text(json.dumps(s, indent=2)+'\n')
print(('Renewed pull access for ' if a.renew_pull_only else 'Published private image and pull access for ')+a.component+'; digest '+digest)
