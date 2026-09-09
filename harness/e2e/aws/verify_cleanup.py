#!/usr/bin/env python3
"""Read back AWS inventory after cleanup and record absence of test resources."""
import argparse
import datetime
import json
import pathlib
import subprocess

p = argparse.ArgumentParser(description=__doc__)
p.add_argument('--work-dir', required=True, type=pathlib.Path)
a = p.parse_args()
path = a.work_dir / 'resources.json'
state = json.loads(path.read_text())
if not state.get('cleanedUp'):
    raise RuntimeError('run cleanup.py before verifying absence')

def aws(service, operation, inputs, absent=()):
    result = subprocess.run(['aws', '--region', state['region'], service, operation,
                             '--cli-input-json', json.dumps(inputs), '--output', 'json'],
                            capture_output=True, text=True)
    if result.returncode:
        if any(code in result.stderr for code in absent):
            return None
        raise RuntimeError(f'{service}/{operation}: lookup failed')
    return json.loads(result.stdout) if result.stdout.strip() else {}

if aws('sts', 'get-caller-identity', {})['Account'] != state['account']:
    raise RuntimeError('AWS account mismatch')
instances = aws('ec2', 'describe-instances', {'Filters': [
    {'Name': 'vpc-id', 'Values': [state['vpcID']]},
    {'Name': 'tag:cloud-provisioning-test', 'Values': [state['runID']]}]})
if any(i['State']['Name'] != 'terminated' for r in instances['Reservations'] for i in r['Instances']):
    raise RuntimeError('a test instance has not terminated')

checks = [('ec2', 'describe-vpcs', {'VpcIds': [state['vpcID']]}, ('InvalidVpcID.NotFound',), 'Vpcs')]
for image in state.get('retiredImages', []):
    checks.append(('ec2', 'describe-images', {'ImageIds': [image['id']]},
                   ('InvalidAMIID.NotFound', 'InvalidAMIID.Unavailable'), 'Images'))
    for snapshot in image.get('snapshots', []):
        checks.append(('ec2', 'describe-snapshots', {'SnapshotIds': [snapshot]},
                       ('InvalidSnapshot.NotFound',), 'Snapshots'))
for name in [state['nodeRoleName'], state['capaRoleName']] + ([state['gatewayRuntimeRoleName']] if state.get('gatewayRuntimeRoleName') else []):
    checks.append(('iam', 'get-role', {'RoleName': name}, ('NoSuchEntity',), 'Role'))
checks.append(('iam', 'get-instance-profile', {'InstanceProfileName': state['instanceProfileName']},
               ('NoSuchEntity',), 'InstanceProfile'))
for key in ('publisherRepository', 'calicoWindowsRepository', 'windowsGPUPluginRepository', 'windowsGPUProbeRepository'):
    if state.get(key):
        checks.append(('ecr', 'describe-repositories', {'repositoryNames': [state[key]['name']]},
                       ('RepositoryNotFoundException',), 'repositories'))
if state.get('assetBucket'):
    checks.append(('s3api', 'get-bucket-tagging', {'Bucket': state['assetBucket']},
                   ('NoSuchBucket',), 'TagSet'))
for service, operation, inputs, absent, key in checks:
    result = aws(service, operation, inputs, absent)
    if result is not None and result.get(key):
        raise RuntimeError(f'{service}/{operation}: test resource still exists')

verification = {'observedAt': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                'allInstancesTerminated': True, 'vpcAbsent': True,
                'recordedImagesAndSnapshotsAbsent': True,
                'rolesAndProfileAbsent': True, 'assetBucketAbsent': True,
                'recordedRepositoriesAbsent': True}
(a.work_dir / 'cleanup-verification.json').write_text(json.dumps(verification, indent=2) + '\n')
print('Verified no active test instances, VPC, recorded images/snapshots/repositories, IAM roles/profile or asset bucket')
