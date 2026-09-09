#!/usr/bin/env python3
"""Publish test binaries to private S3 objects and return expiring download URLs.

Run with setup credentials. URLs are written privately, never printed. The
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
policy = {'Version': '2012-10-17', 'Statement': [{'Effect': 'Allow', 'Action': ['s3:GetObject'], 'Resource': f'arn:aws:s3:::{bucket}/downloads/*'}]}
# A restricted session signs the URLs; the source identity itself never reaches
# a VM or a Kubernetes Secret. URLs remain bearer credentials for these objects.
session = json.loads(aws('sts', 'get-federation-token', '--name', 'cldt-assets', '--duration-seconds', '14400', '--policy', json.dumps(policy)))['Credentials']
env = dict(os.environ, AWS_ACCESS_KEY_ID=session['AccessKeyId'], AWS_SECRET_ACCESS_KEY=session['SecretAccessKey'], AWS_SESSION_TOKEN=session['SessionToken'])
urls = {}
for value in a.file:
    source = pathlib.Path(value)
    key = 'downloads/' + source.name
    aws('s3api', 'put-object', '--bucket', bucket, '--key', key, '--body', str(source), '--server-side-encryption', 'AES256')
    urls[source.name] = aws('s3', 'presign', f's3://{bucket}/{key}', '--expires-in', '14400', env=env).strip()
(work / 'asset-urls.json').write_text(json.dumps(urls, indent=2) + '\n')
print('Published', len(urls), 'private objects; download URLs saved in asset-urls.json')
