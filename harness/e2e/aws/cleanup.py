#!/usr/bin/env python3
"""Delete only the isolated test run recorded by bootstrap.py.

Source source-me.sh first. Instance termination is restricted to the recorded
VPC AND run tag. Keep the state file as an audit trail after cleanup.
"""
import argparse
import json
import pathlib
import subprocess

p = argparse.ArgumentParser()
p.add_argument('--work-dir', required=True)
a = p.parse_args()
path = pathlib.Path(a.work_dir) / 'resources.json'
s = json.loads(path.read_text())
if not s['runID'].startswith('cldt-'):
    p.error('invalid test run ID')

def aws(*args, absent_ok=False):
    r = subprocess.run(['aws', '--region', s['region'], *args, '--output', 'json'], capture_output=True, text=True)
    if r.returncode:
        if absent_ok and any(x in r.stderr for x in ('NotFound', 'NoSuchEntity', 'NoSuchBucket', 'InvalidAssociationID.NotFound')):
            return {}
        raise RuntimeError(r.stderr.strip())
    return json.loads(r.stdout) if r.stdout.strip() else {}

if aws('sts', 'get-caller-identity')['Account'] != s['account']:
    raise RuntimeError('AWS account does not match recorded test account')
for arn in s.get('bootstrapProbeSecrets', []):
    secret = aws('secretsmanager', 'describe-secret', '--secret-id', arn, absent_ok=True)
    if secret:
        if {t['Key']: t['Value'] for t in secret.get('Tags', [])}.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('bootstrap probe secret ownership mismatch')
        aws('secretsmanager', 'delete-secret', '--secret-id', arn, '--force-delete-without-recovery', absent_ok=True)
vpc = s.get('vpcID')
if vpc:
    result = aws('ec2', 'describe-vpcs', '--vpc-ids', vpc, absent_ok=True)
    if result:
        tags = {t['Key']: t['Value'] for t in result['Vpcs'][0].get('Tags', [])}
        if tags.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('VPC ownership tag does not match; refusing cleanup')
        # CAPA may already have requested termination. Include shutting-down
        # instances so their ENIs disappear before deleting the network.
        instances = aws('ec2', 'describe-instances', '--filters', f'Name=vpc-id,Values={vpc}', f'Name=tag:cloud-provisioning-test,Values={s["runID"]}', 'Name=instance-state-name,Values=pending,running,stopping,stopped,shutting-down')
        ids = [i['InstanceId'] for r in instances['Reservations'] for i in r['Instances']]
        if ids:
            aws('ec2', 'terminate-instances', '--instance-ids', *ids)
            aws('ec2', 'wait', 'instance-terminated', '--instance-ids', *ids)
        for key, command, flag in [('securityGroupID', 'delete-security-group', '--group-id'), ('routeAssociationID', 'disassociate-route-table', '--association-id'), ('routeTableID', 'delete-route-table', '--route-table-id'), ('subnetID', 'delete-subnet', '--subnet-id')]:
            if s.get(key):
                aws('ec2', command, flag, s[key], absent_ok=True)
        if s.get('internetGatewayID'):
            aws('ec2', 'detach-internet-gateway', '--internet-gateway-id', s['internetGatewayID'], '--vpc-id', vpc, absent_ok=True)
            aws('ec2', 'delete-internet-gateway', '--internet-gateway-id', s['internetGatewayID'], absent_ok=True)
        aws('ec2', 'delete-vpc', '--vpc-id', vpc, absent_ok=True)
image_map = {image['id']: image for image in s.get('retiredImages', [])}
if s.get('preparedAMIID') and s['preparedAMIID'] not in image_map:
    image_map[s['preparedAMIID']] = {'id': s['preparedAMIID'], 'snapshots': s.get('preparedSnapshotIDs', [])}
images = list(image_map.values())
for owned in images:
    result = aws('ec2', 'describe-images', '--image-ids', owned['id'], absent_ok=True)
    for image in result.get('Images', []):
        if {t['Key']: t['Value'] for t in image.get('Tags', [])}.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('prepared AMI ownership tag mismatch')
        owned['snapshots'] = [m['Ebs']['SnapshotId'] for m in image.get('BlockDeviceMappings', []) if 'Ebs' in m]
        s['retiredImages'] = images
        path.write_text(json.dumps(s, indent=2) + '\n')
        aws('ec2', 'deregister-image', '--image-id', owned['id'], absent_ok=True)
    for snapshot in owned.get('snapshots', []):
        result = aws('ec2', 'describe-snapshots', '--snapshot-ids', snapshot, absent_ok=True)
        for item in result.get('Snapshots', []):
            if {t['Key']: t['Value'] for t in item.get('Tags', [])}.get('cloud-provisioning-test') != s['runID']:
                raise RuntimeError('prepared snapshot ownership tag mismatch')
            aws('ec2', 'delete-snapshot', '--snapshot-id', snapshot, absent_ok=True)
for key in ('publisherRepository', 'calicoWindowsRepository', 'windowsGPUPluginRepository', 'windowsGPUProbeRepository'):
    if not s.get(key):
        continue
    repo = s[key]
    result = aws('ecr', 'describe-repositories', '--repository-names', repo['name'], absent_ok=True)
    if result:
        actual = result['repositories'][0]
        if actual['repositoryArn'] != repo['arn']:
            raise RuntimeError('publisher repository ARN mismatch')
        tags = aws('ecr', 'list-tags-for-resource', '--resource-arn', repo['arn'])['tags']
        if {t['Key']: t['Value'] for t in tags}.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('publisher repository ownership mismatch')
        aws('ecr', 'delete-repository', '--repository-name', repo['name'], '--force', absent_ok=True)
if s.get('assetBucket'):
    tags = aws('s3api', 'get-bucket-tagging', '--bucket', s['assetBucket'], absent_ok=True)
    if tags:
        if {t['Key']: t['Value'] for t in tags['TagSet']}.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('asset bucket ownership tag does not match')
        objects = aws('s3api', 'list-objects-v2', '--bucket', s['assetBucket']).get('Contents', [])
        for obj in objects:
            aws('s3api', 'delete-object', '--bucket', s['assetBucket'], '--key', obj['Key'])
        aws('s3api', 'delete-bucket', '--bucket', s['assetBucket'])
if s.get('instanceProfileName'):
    if s.get('nodeRoleName'):
        aws('iam', 'remove-role-from-instance-profile', '--instance-profile-name', s['instanceProfileName'], '--role-name', s['nodeRoleName'], absent_ok=True)
    aws('iam', 'delete-instance-profile', '--instance-profile-name', s['instanceProfileName'], absent_ok=True)
if s.get('nodeRoleName'):
    aws('iam', 'delete-role-policy', '--role-name', s['nodeRoleName'], '--policy-name', 'cldt-test', absent_ok=True)
    aws('iam', 'detach-role-policy', '--role-name', s['nodeRoleName'], '--policy-arn', 'arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore', absent_ok=True)
    aws('iam', 'delete-role', '--role-name', s['nodeRoleName'], absent_ok=True)
if s.get('gatewayRuntimeRoleName'):
    role = aws('iam', 'get-role', '--role-name', s['gatewayRuntimeRoleName'], absent_ok=True).get('Role')
    if role:
        tags = {t['Key']: t['Value'] for t in role.get('Tags', [])}
        if role['Arn'] != s.get('gatewayRuntimeRoleARN') or tags.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('gateway runtime role ownership mismatch')
        for policy in ('gateway-routes', 'gateway-source-check', 'gateway-ingress', 'gateway-observation'):
            aws('iam', 'delete-role-policy', '--role-name', s['gatewayRuntimeRoleName'], '--policy-name', policy, absent_ok=True)
        aws('iam', 'delete-role', '--role-name', s['gatewayRuntimeRoleName'], absent_ok=True)
if s.get('capaRoleName'):
    aws('iam', 'delete-role-policy', '--role-name', s['capaRoleName'], '--policy-name', 'cldt-test', absent_ok=True)
    aws('iam', 'delete-role', '--role-name', s['capaRoleName'], absent_ok=True)
session = path.parent / 'capa-session.json'
session.unlink(missing_ok=True)
(path.parent / 'harness-session.json').unlink(missing_ok=True)
(path.parent / 'asset-urls.json').unlink(missing_ok=True)
s['cleanedUp'] = True
path.write_text(json.dumps(s, indent=2) + '\n')
print('Cleaned up ' + s['runID'])
