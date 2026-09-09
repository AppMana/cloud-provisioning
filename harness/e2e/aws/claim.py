#!/usr/bin/env python3
"""Apply a real CAPA worker claim against an observed, imported VM site.

No RunInstances call or Machine/Node status synthesis occurs here. The external
infrastructure and imported control-plane status describe resources owned by
the test setup; CAPA and CAPI reconcile the worker and its Node association.
"""
import argparse
import json
import pathlib
import subprocess

p = argparse.ArgumentParser()
p.add_argument('--work-dir', required=True)
p.add_argument('--name', default='aws-remote1')
p.add_argument('--machine-template', help='new immutable template name; defaults to the claim name')
p.add_argument('--api-server', required=True)
p.add_argument('--bastion', default='clab-cldt-bastion')
p.add_argument('--instance-type')
p.add_argument('--linux-image-id', help='explicit authorized Linux image; leaves the run default unchanged')
p.add_argument('--variant', choices=['cpu','gpu'], default='cpu', help='Windows image variant; GPU requires an explicit instance type')
p.add_argument('--root-volume-gib', type=int)
p.add_argument('--windows-version', choices=['2022', '2025'], help='Use this run’s captured, available Windows image')
a = p.parse_args()
if a.linux_image_id and a.windows_version:
    p.error('--linux-image-id cannot select a Windows machine')
template_name = a.machine_template or a.name
if a.variant == 'gpu' and (not a.windows_version or not a.instance_type):
    p.error('GPU Windows claims require --windows-version and --instance-type')
a.instance_type = a.instance_type or 't3.large'
minimum_volume = 60 if a.variant == 'gpu' else (50 if a.windows_version else 20)
s = json.loads((pathlib.Path(a.work_dir) / 'resources.json').read_text())
if a.windows_version:
    image = s.get('windowsGpuImageBuilds' if a.variant == 'gpu' else 'windowsImageBuilds', {}).get(a.windows_version, {})
    if image.get('imageState') != 'available' or not image.get('imageID'):
        p.error('Windows image must be captured and available; refresh windows_image.py --phase status')
    if image.get('cacheRecipe') and (
            image['cacheRecipe'].get('schemaVersion') not in (2, 3) or
            image['cacheRecipe'].get('dataDirectoryPermissions') != 'inherit-parent' or
            image.get('cacheVerification', {}).get('passed') is not True or
            image.get('cacheVerification', {}).get('checks', {}).get('dataDirectoryInheritsPermissions') is not True or
            (image['cacheRecipe'].get('schemaVersion') == 3 and
             image.get('cacheVerification', {}).get('checks', {}).get('exactImageTargets') is not True)):
        p.error('Cached Windows image requires the inherited-permissions recipe and native verification; rebuild the image')
    image_minimum = image.get('rootVolumeGiB', minimum_volume)
    if type(image_minimum) is not int or image_minimum < 1:
        p.error('Invalid captured image root-volume size; refresh image status')
    minimum_volume = max(minimum_volume, image_minimum)
    ami_id = image['imageID']
    machine_os = 'windows'
else:
    ami_id = a.linux_image_id or s['amiID']
    if a.linux_image_id and ami_id not in s.get('authorizedLinuxAMIs', []):
        p.error('Explicit Linux image must be authorized with bake.py authorize first')
    if ami_id == s.get('baseAMIID') and ami_id != s.get('preparedAMIID'):
        p.error('This run still selects its unprepared Ubuntu base AMI; run bake.py start, capture and promote before a Linux CAPA claim')
    machine_os = 'linux'
if a.root_volume_gib is None:
    a.root_volume_gib = minimum_volume if a.windows_version else 40
if a.root_volume_gib < minimum_volume:
    p.error(f'root-volume-gib must be at least {minimum_volume}')
ns = 'cloud-provisioning'
cluster = s['runID']
def kube(*args, body=None, private=False):
    r = subprocess.run(['docker', 'exec', '-i', a.bastion, 'kubectl', '--server='+a.api_server, *args], input=body, capture_output=True, text=True)
    if r.returncode:
        raise RuntimeError('credential publication failed' if private else r.stderr.strip())
    return r.stdout
def apply(obj, private=False):
    kube('apply', '-f', '-', body=json.dumps(obj), private=private)
def obj(api, kind, name, spec):
    return {'apiVersion': api, 'kind': kind, 'metadata': {'name': name, 'namespace': ns}, 'spec': spec}
infra = obj('infrastructure.cluster.x-k8s.io/v1beta2', 'AWSCluster', cluster, {
    'region': s['region'], 'identityRef': {'kind': 'AWSClusterStaticIdentity', 'name': cluster},
    'controlPlaneEndpoint': {'host': a.api_server.removeprefix('https://').rsplit(':', 1)[0], 'port': int(a.api_server.rsplit(':', 1)[1])},
    'network': {'vpc': {'id': s['vpcID']}, 'subnets': [{'id': s['subnetID'], 'isPublic': True}]},
})
infra['metadata']['annotations'] = {'cluster.x-k8s.io/managed-by': 'external'}
apply(infra)
kube('-n', ns, 'patch', 'awscluster', cluster, '--subresource=status', '--type=merge', '-p', json.dumps({'status': {'ready': True}}))
# cmd/importsite observes the live site and creates this target's connection.
observed = json.loads(kube('-n', ns, 'get', 'importedcontrolplane', cluster, '-o', 'json'))
if not observed.get('status', {}).get('initialization', {}).get('controlPlaneInitialized'):
    raise RuntimeError('run cmd/importsite for this cluster before creating claims')
kube('-n', ns, 'get', 'secret', cluster+'-kubeconfig', '-o', 'name')
apply(obj('cluster.x-k8s.io/v1beta2', 'Cluster', cluster, {
    'controlPlaneRef': {'apiGroup': 'containernet.appmana.com', 'kind': 'ImportedControlPlane', 'name': cluster},
    'infrastructureRef': {'apiGroup': 'infrastructure.cluster.x-k8s.io', 'kind': 'AWSCluster', 'name': cluster},
}))
apply(obj('infrastructure.cluster.x-k8s.io/v1beta2', 'AWSMachineTemplate', template_name, {'template': {'metadata': {'labels': {'kubernetes.io/os': machine_os}}, 'spec': {
    'instanceType': a.instance_type, 'ami': {'id': ami_id}, 'subnet': {'id': s['subnetID']},
    'additionalSecurityGroups': [{'id': s['securityGroupID']}], 'publicIP': True,
    'sshKeyName': '', 'iamInstanceProfile': s['instanceProfileName'],
    'additionalTags': {'cloud-provisioning-test': s['runID']},
    'cloudInit': {'insecureSkipSecretsManager': False},
    'rootVolume': {'size': a.root_volume_gib, 'type': 'gp3', 'encrypted': True},
}}}))
apply(obj('cloud-provisioning.appmana.com/v1alpha1', 'ProvisionedNodeClaim', a.name, {
    'infrastructureRef': {'apiGroup': 'infrastructure.cluster.x-k8s.io', 'kind': 'AWSMachineTemplate', 'name': template_name}, 'clusterName': cluster,
}))
print('Applied CAPA claim', a.name)
