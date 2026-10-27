#!/usr/bin/env python3
"""Publish test binaries to private S3 objects as s3:// artifact sources.

Run with setup credentials. The controller signs the sources when it renders a
bootstrap; this grants the run's controller identity read access to them. The
bucket is recorded before uploads so cleanup can handle interrupted setup.
"""
import argparse
import json
import os
import pathlib
import subprocess

p = argparse.ArgumentParser()
p.add_argument('--work-dir', required=True)
p.add_argument('--file', action='append', required=True)
a = p.parse_args()
os.umask(0o077)
work = pathlib.Path(a.work_dir)
path = work / 'resources.json'
s = json.loads(path.read_text())
def aws(*args, env=None):
    r = subprocess.run(['aws', '--region', s['region'], *args], capture_output=True, text=True, env=env)
    if r.returncode:
        raise RuntimeError('AWS asset operation failed: ' + r.stderr.strip())
    return r.stdout
if json.loads(aws('sts', 'get-caller-identity'))['Account'] != s['account']:
    raise RuntimeError('AWS account mismatch')
bucket = s.get('assetBucket')
if not bucket:
    bucket = s['runID'] + '-' + s['account'] + '-assets'
    args = ['s3api', 'create-bucket', '--bucket', bucket]
    if s['region'] != 'us-east-1':
        args += ['--create-bucket-configuration', 'LocationConstraint=' + s['region']]
    aws(*args)
    s['assetBucket'] = bucket
    path.write_text(json.dumps(s, indent=2) + '\n')
    aws('s3api', 'put-public-access-block', '--bucket', bucket, '--public-access-block-configuration', 'BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true')
    aws('s3api', 'put-bucket-tagging', '--bucket', bucket, '--tagging', json.dumps({'TagSet': [{'Key': 'cloud-provisioning-test', 'Value': s['runID']}]}))
# The controller signs these sources itself when it renders a bootstrap, with
# the run's controller identity, which reads only the downloads/ prefix.
iam = pathlib.Path(__file__).resolve().parents[3] / 'docs' / 'iam'
policy = (iam / 'controller-artifacts.json').read_text().replace('${ASSET_BUCKET}', bucket).replace('${ASSET_PREFIX}', 'downloads/')
aws('iam', 'put-user-policy', '--user-name', s['controllerUserName'], '--policy-name', 'cldt-artifacts', '--policy-document', json.dumps(json.loads(policy)))
sources = {}
for value in a.file:
    source = pathlib.Path(value)
    key = 'downloads/' + source.name
    aws('s3api', 'put-object', '--bucket', bucket, '--key', key, '--body', str(source), '--server-side-encryption', 'AES256')
    sources[source.name] = f's3://{bucket}/{key}'
(work / 'asset-urls.json').write_text(json.dumps(sources, indent=2) + '\n')
print('Published', len(sources), 'private objects; their s3:// sources are in asset-urls.json')
