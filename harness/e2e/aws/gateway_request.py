#!/usr/bin/env python3
"""Create one UID-bound AWS gateway request from an existing topology template."""
import argparse
import copy
import ipaddress
import json
import os
import pathlib
import subprocess


def binding(state, machine, node, instance, subnet, expected_uid):
    """Bind the same native AWS Machine model for Linux and Windows workers."""
    provider = machine['spec'].get('providerID', '')
    if (state.get('cleanedUp') or not state['runID'].startswith('cldt-') or
            machine['metadata']['uid'] != expected_uid or
            machine['metadata'].get('deletionTimestamp') or node['metadata'].get('deletionTimestamp') or
            machine['spec']['clusterName'] != state['runID'] or
            machine.get('status', {}).get('nodeRef', {}).get('name') != node['metadata']['name'] or
            not node['metadata'].get('uid') or node['spec'].get('providerID') != provider or
            not provider.startswith('aws:///') or provider.rsplit('/', 1)[-1] != instance['InstanceId']):
        raise ValueError('Machine, Node and instance identities do not match the active run')
    if (instance['VpcId'] != state['vpcID'] or instance['SubnetId'] != state['subnetID'] or
            subnet['SubnetId'] != state['subnetID'] or subnet['VpcId'] != state['vpcID'] or
            instance['State']['Name'] != 'running' or len(instance['NetworkInterfaces']) != 1 or
            {t['Key']: t['Value'] for t in instance['Tags']}.get('cloud-provisioning-test') != state['runID']):
        raise ValueError('Require an owned, running, single-NIC instance in the test subnet')
    interface = instance['NetworkInterfaces'][0]
    if (interface.get('Attachment', {}).get('DeviceIndex') != 0 or
            interface['VpcId'] != state['vpcID'] or interface['SubnetId'] != state['subnetID'] or
            interface['PrivateIpAddress'] != instance['PrivateIpAddress'] or
            ipaddress.ip_address(instance['PrivateIpAddress']) not in ipaddress.ip_network(subnet['CidrBlock'])):
        raise ValueError('Primary interface and subnet observations disagree')
    return dict(uid=expected_uid, nodeUID=node['metadata']['uid'], providerID=provider,
                interfaceID=interface['NetworkInterfaceId'],
                networkID='aws:'+state['account']+':'+state['region']+':'+state['vpcID'],
                subnet=subnet['CidrBlock'], address=instance['PrivateIpAddress'])


def request_configmap(template, worker, gateway, worker_name, underlay):
    if template['gateway'] != gateway:
        raise ValueError('Template gateway no longer matches its observed binding')
    if worker['uid'] == gateway['uid'] or worker['nodeUID'] == gateway['nodeUID']:
        raise ValueError('Worker and gateway must be different machines')
    request = copy.deepcopy(template)
    request['worker'] = worker
    request['underlay'] = sorted(set(underlay))
    return {'apiVersion': 'v1', 'kind': 'ConfigMap',
            'metadata': {'name': 'gateway-'+worker_name, 'namespace': 'cloud-provisioning',
                         'labels': {'cloud-provisioning.appmana.com/gateway-request': 'cloud-provisioning-peers'}},
            'data': {'request.json': json.dumps(request)}}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ['work-dir', 'intent']:
        p.add_argument('--'+name, type=pathlib.Path, required=True)
    for name in ['api-server', 'worker', 'worker-uid', 'gateway', 'gateway-uid', 'template-configmap']:
        p.add_argument('--'+name, required=True)
    p.add_argument('--bastion', default='clab-cldt-bastion')
    a = p.parse_args()
    os.umask(0o077)
    if a.intent.exists():
        p.error('intent exists; inspect the existing operation before another submission')
    state = json.loads((a.work_dir/'resources.json').read_text())
    credentials = json.loads((a.work_dir/'harness-session.json').read_text())['Credentials']
    env = dict(os.environ, AWS_ACCESS_KEY_ID=credentials['AccessKeyId'],
               AWS_SECRET_ACCESS_KEY=credentials['SecretAccessKey'], AWS_SESSION_TOKEN=credentials['SessionToken'])
    env.pop('AWS_SECURITY_TOKEN', None)
    command = ['docker', 'exec', '-i', a.bastion, 'kubectl', '--server='+a.api_server]

    def kube(*args):
        return json.loads(subprocess.check_output(command+list(args)+['-o', 'json']))

    def aws(operation, **inputs):
        result = subprocess.run(['aws', '--region', state['region'], 'ec2', operation,
                                 '--cli-input-json', json.dumps(inputs), '--output', 'json'],
                                env=env, capture_output=True, text=True, timeout=60)
        if result.returncode:
            raise RuntimeError('EC2 observation failed: '+operation)
        return json.loads(result.stdout)

    subnet = aws('describe-subnets', SubnetIds=[state['subnetID']])['Subnets'][0]

    def observe(name, uid):
        machine = kube('-n', 'cloud-provisioning', 'get', 'machine', name)
        node = kube('get', 'node', machine['status']['nodeRef']['name'])
        instance_id = machine['spec']['providerID'].rsplit('/', 1)[-1]
        instances = [i for r in aws('describe-instances', InstanceIds=[instance_id])['Reservations'] for i in r['Instances']]
        if len(instances) != 1:
            raise ValueError('Ambiguous EC2 instance observation')
        return binding(state, machine, node, instances[0], subnet, uid)

    template = json.loads(kube('-n', 'cloud-provisioning', 'get', 'configmap', a.template_configmap)['data']['request.json'])
    worker = observe(a.worker, a.worker_uid)
    gateway = observe(a.gateway, a.gateway_uid)
    machines = kube('-n', 'cloud-provisioning', 'get', 'machines')['items']
    # The fake-VM and AWS CAPI Cluster records share this imported workload
    # site and peer namespace. Keep both providers' endpoint escape addresses.
    underlay = [address['address'] for m in machines for address in m.get('status', {}).get('addresses', [])
                if address['type'] == 'ExternalIP' and ':' not in address['address']]
    cm = request_configmap(template, worker, gateway, a.worker, underlay)
    # Recheck both bindings immediately before recording intent and creating the
    # request. The controller independently enforces these exact identities.
    if observe(a.worker, a.worker_uid) != worker or observe(a.gateway, a.gateway_uid) != gateway:
        raise ValueError('Binding changed during observation')
    with a.intent.open('x') as stream:
        json.dump(cm, stream, indent=2)
        stream.write('\n')
    result = subprocess.run(command+['create', '-f', '-'], input=json.dumps(cm), capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('Request creation failed; inspect the saved intent and cluster before retrying')
    print('Created UID-bound gateway request '+cm['metadata']['name'])


if __name__ == '__main__':
    main()
