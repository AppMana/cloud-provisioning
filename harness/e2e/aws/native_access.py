#!/usr/bin/env python3
"""Configure scoped IPAM and observer access for an isolated native CNI campaign."""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess


def node_policy(state):
    region, account = state['region'], state['account']
    return {'Version': '2012-10-17', 'Statement': [
        {'Effect': 'Allow', 'Action': ['ec2:DescribeNetworkInterfaces', 'ec2:DescribeInstances',
            'ec2:DescribeInstanceTypes', 'ec2:DescribeTags', 'ec2:DescribeSubnets'],
         'Resource': '*', 'Condition': {'StringEquals': {'aws:RequestedRegion': region}}},
        {'Effect': 'Allow', 'Action': ['ec2:AssignPrivateIpAddresses', 'ec2:UnassignPrivateIpAddresses'],
         'Resource': f'arn:aws:ec2:{region}:{account}:network-interface/*',
         'Condition': {'StringEquals': {'aws:RequestedRegion': region,
                       'ec2:ResourceTag/cloud-provisioning-test': state['runID']}}},
        {'Effect': 'Allow', 'Action': ['ec2:CreateTags'],
         'Resource': f'arn:aws:ec2:{region}:{account}:network-interface/*',
         'Condition': {'StringEquals': {'aws:RequestedRegion': region,
                       'ec2:ResourceTag/cloud-provisioning-test': state['runID']},
                       'ForAllValues:StringEquals': {'aws:TagKeys': ['node.k8s.amazonaws.com/instance_id']}}},
    ]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--runner', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    path = args.work_dir / 'resources.json'
    state = json.loads(path.read_text())
    if state.get('cleanedUp') or not state['runID'].startswith('cldt-'):
        raise RuntimeError('require an active isolated run')

    def aws(service, operation, **parameters):
        result = subprocess.run(['aws', '--region', state['region'], service, operation,
                                 '--cli-input-json', json.dumps(parameters), '--output', 'json'],
                                capture_output=True, text=True, check=True)
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def owned(obj):
        if {tag['Key']: tag['Value'] for tag in obj.get('Tags', [])}.get('cloud-provisioning-test') != state['runID']:
            raise RuntimeError('resource ownership mismatch')

    if aws('sts', 'get-caller-identity')['Account'] != state['account']:
        raise RuntimeError('account mismatch')
    vpc = aws('ec2', 'describe-vpcs', VpcIds=[state['vpcID']])['Vpcs'][0]
    role = aws('iam', 'get-role', RoleName=state['nodeRoleName'])['Role']
    subnet = aws('ec2', 'describe-subnets', SubnetIds=[state['subnetID']])['Subnets'][0]
    group = aws('ec2', 'describe-security-groups', GroupIds=[state['securityGroupID']])['SecurityGroups'][0]
    for obj in (vpc, role, subnet, group):
        owned(obj)
    if subnet['VpcId'] != vpc['VpcId'] or group['VpcId'] != vpc['VpcId'] or role['Arn'] != state['nodeRoleARN']:
        raise RuntimeError('inventory bindings differ from AWS')
    policy = node_policy(state)
    aws('iam', 'put-role-policy', RoleName=state['nodeRoleName'], PolicyName='cldt-native-cni', PolicyDocument=json.dumps(policy))
    (args.work_dir / 'native-cni-policy.json').write_text(json.dumps(policy, indent=2))
    state['nativeCNI'] = {'family': 'ipv4', 'maxENI': 1, 'policyName': 'cldt-native-cni'}
    path.write_text(json.dumps(state, indent=2) + '\n')
    # Native VPC pod-to-pod traffic stays inside this run-owned security group.
    if not any(rule['IpProtocol'] == '-1' and any(pair.get('GroupId') == state['securityGroupID'] for pair in rule.get('UserIdGroupPairs', [])) for rule in group['IpPermissions']):
        aws('ec2', 'authorize-security-group-ingress', GroupId=state['securityGroupID'],
            IpPermissions=[{'IpProtocol': '-1', 'UserIdGroupPairs': [{'GroupId': state['securityGroupID']}]}])
    observer_policy = {'Version': '2012-10-17', 'Statement': [{'Effect': 'Allow',
        'Action': 'ec2:DescribeNetworkInterfaces', 'Resource': '*',
        'Condition': {'StringEquals': {'aws:RequestedRegion': state['region']}}}]}
    credentials = aws('sts', 'get-federation-token', Name='cldt-pod-observer', DurationSeconds=3600,
                      Policy=json.dumps(observer_policy))['Credentials']
    (args.work_dir / 'observer-session.json').write_text(json.dumps(credentials))
    scope = {'account': state['account'], 'region': state['region'], 'vpc': state['vpcID'],
             'subnets': [state['subnetID']], 'namespace': 'cloud-provisioning', 'cluster': state['runID'],
             'ownerTag': 'cloud-provisioning-test', 'ownerValue': state['runID'], 'family': 'ipv4',
             'podRanges': [subnet['CidrBlock']]}
    objects = [
        {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'native-pod-scope', 'namespace': 'cloud-provisioning'},
         'data': {'scope.json': json.dumps(scope)}},
        {'apiVersion': 'v1', 'kind': 'Secret', 'metadata': {'name': 'native-pod-observer', 'namespace': 'cloud-provisioning'},
         'type': 'Opaque', 'data': {name: base64.b64encode(credentials[key].encode()).decode()
          for name, key in {'AWS_ACCESS_KEY_ID': 'AccessKeyId', 'AWS_SECRET_ACCESS_KEY': 'SecretAccessKey',
                            'AWS_SESSION_TOKEN': 'SessionToken'}.items()}},
    ]
    subprocess.run(['docker', 'exec', '-i', args.runner, 'docker', 'exec', '-i', 'clab-cldt-bastion',
                    'kubectl', '--server=https://10.101.0.1:6443', 'apply', '-f', '-'],
                   input=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': objects}),
                   check=True, text=True, capture_output=True)
    print('Configured single-ENI IPv4 IPAM and read-only allocation observation for the isolated run')


if __name__ == '__main__':
    main()
