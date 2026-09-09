"""Validate the identity and startup stage of a GPU probe without GPU assignment.

A process-start result is not GPU qualification. HCS events must come from the
captured control VM; container identity binds the selected event stream.
"""
import re
import json
from windows_gpu import IMAGE, COMMAND, GPU, validate_image
from windows_hcs import timeline


def evaluate(node, pod, job, node_uid, pod_uid, job_uid, events, image=IMAGE):
    validate_image(image)
    if (not node_uid or not pod_uid or not job_uid or
            node['metadata']['uid'] != node_uid or pod['metadata']['uid'] != pod_uid or
            job['metadata']['uid'] != job_uid or pod['spec'].get('nodeName') != node['metadata']['name']):
        raise ValueError('Control Node, Pod or Job identity changed')
    owners = [o for o in pod['metadata'].get('ownerReferences', []) if o.get('controller') is True]
    if len(owners) != 1 or owners[0].get('kind') != 'Job' or owners[0].get('uid') != job_uid or owners[0].get('name') != job['metadata']['name']:
        raise ValueError('Control Pod is not owned by the expected Job')
    spec = pod['spec']
    containers = spec.get('containers', [])
    if (node['metadata'].get('labels', {}).get('kubernetes.io/os') != 'windows' or
            len(containers) != 1 or spec.get('initContainers') or spec.get('ephemeralContainers') or spec.get('hostNetwork')):
        raise ValueError('Require one ordinary Windows control container')
    c = containers[0]
    contexts = [spec.get('securityContext', {}), c.get('securityContext', {})]
    if any(x.get('windowsOptions', {}).get('hostProcess') for x in contexts):
        raise ValueError('HostProcess cannot qualify an ordinary-container control')
    if c.get('image') != image or c.get('command') != COMMAND or c.get('args'):
        raise ValueError('Control probe image or command differs')
    if any(GPU in c.get('resources', {}).get(k, {}) for k in ['limits', 'requests']):
        raise ValueError('Control must have no GPU resource assignment')
    hcs = timeline(pod, events, c['name'])
    prefix = '[' + hcs['containerID'] + '] '
    starts = []
    specifications = []
    for event in events:
        message = event.get('message', '')
        if event.get('id') == 2010 and message.startswith(prefix):
            if "settings '" not in message or not message.endswith("'"):
                raise ValueError('Incomplete HCS creation specification')
            specification = json.loads(message.split("settings '", 1)[1][:-1])
            if not isinstance(specification.get('Container'), dict):
                raise ValueError('HCS event is not an ordinary container specification')
            specifications.append(specification['Container'])
        if event.get('id') == 2500 and message.startswith(prefix):
            result = re.search(r'result (0x[0-9a-fA-F]{8}), process ID ([0-9]+)', message)
            if result:
                starts.append({'result': result.group(1).lower(), 'processID': int(result.group(2))})
    if len(specifications) != 1 or specifications[0].get('AssignedDevices'):
        raise ValueError('Require one native HCS creation specification without assigned devices')
    if len(starts) > 1:
        raise ValueError('Multiple HCS process attempts cannot qualify this control')
    created = any(s['result'] == '0x00000000' and s['processID'] > 0 for s in starts)
    observation = 'observed-success' if created else ('observed-failure' if any(s['result'] != '0x00000000' for s in starts) else 'unobserved')
    return {'image': image, 'nodeUID': node_uid, 'podUID': pod_uid, 'jobUID': job_uid,
            'noGPUAssignment': True, 'processCreated': created, 'processCreationObservation': observation,
            'createProcessResults': starts, 'timeline': hcs,
            'scope': 'Identity-bound process control without GPU assignment; first-workload eligibility is a separate preflight. No GPU rendering or root-cause claim.'}
