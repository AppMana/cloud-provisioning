#!/usr/bin/env python3
"""Make an isolated run's AWS ready for AWS VPC CNI workers, as an operator does.

Two documented operator steps (docs/aws.md): attach the published VPC CNI node
policy to the worker role, rendered for the run's cluster, and give the run's
security group an ingress rule from itself so pods on different workers reach
each other. Everything else the VPC CNI needs is the product's.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess

IAM = Path(__file__).resolve().parents[3] / 'docs' / 'iam'


def node_policy(state, family):
    """The published VPC CNI node policy for this run's cluster. The harness
    names the CAPI Cluster after the run."""
    if family not in ('ipv4', 'ipv6'):
        raise ValueError('require an explicit native pod family')
    raw = (IAM / f'vpc-cni-node-{family}.json').read_text()
    for key, value in {'AWS_REGION': state['region'], 'AWS_ACCOUNT_ID': state['account'], 'CLUSTER_NAME': state['runID']}.items():
        raw = raw.replace('${' + key + '}', value)
    if '${' in raw:
        raise ValueError('unrendered policy variable')
    return json.loads(raw)


def self_admitting(group):
    return any(rule['IpProtocol'] == '-1' and any(pair.get('GroupId') == group['GroupId'] for pair in rule.get('UserIdGroupPairs', []))
               for rule in group['IpPermissions'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--family', choices=('ipv4', 'ipv6'), default='ipv4')
    args = parser.parse_args()
    os.umask(0o077)
    path = args.work_dir / 'resources.json'
    state = json.loads(path.read_text())
    if state.get('cleanedUp') or not state['runID'].startswith('cldt-'):
        raise RuntimeError('require an active isolated run')
    if state.get('nativeCNI', {}).get('family', args.family) != args.family:
        raise RuntimeError('pod family changes require a separate native CNI campaign')

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
    role = aws('iam', 'get-role', RoleName=state['nodeRoleName'])['Role']
    group = aws('ec2', 'describe-security-groups', GroupIds=[state['securityGroupID']])['SecurityGroups'][0]
    for obj in (role, group):
        owned(obj)
    if group['VpcId'] != state['vpcID'] or role['Arn'] != state['nodeRoleARN']:
        raise RuntimeError('inventory bindings differ from AWS')
    policy = node_policy(state, args.family)
    aws('iam', 'put-role-policy', RoleName=state['nodeRoleName'], PolicyName='cldt-native-cni', PolicyDocument=json.dumps(policy))
    (args.work_dir / 'native-cni-policy.json').write_text(json.dumps(policy, indent=2))
    state['nativeCNI'] = {'family': args.family, 'policyName': 'cldt-native-cni'}
    path.write_text(json.dumps(state, indent=2) + '\n')
    if not self_admitting(group):
        aws('ec2', 'authorize-security-group-ingress', GroupId=state['securityGroupID'],
            IpPermissions=[{'IpProtocol': '-1', 'UserIdGroupPairs': [{'GroupId': state['securityGroupID']}]}])
    print('Attached the ' + args.family + ' VPC CNI node policy and the security group self rule for the isolated run')


if __name__ == '__main__':
    main()
