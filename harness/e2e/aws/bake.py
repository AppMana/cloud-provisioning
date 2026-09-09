#!/usr/bin/env python3
"""Prepare a disposable EC2 image builder; CAPA still launches test workers.

Run start and capture, then authorize a candidate for fresh-worker testing.
Authorization extends the scoped CAPA image grant; promote changes the default
worker AMI. Inventory makes interrupted preparation stages resumable.
"""
import argparse
import json
import hashlib
import os
import pathlib
import shlex
import subprocess
import time

from linux_recipe import validate_shell
from image_policy import owned_image, retained_linux_arns, linux_builder_parent

p = argparse.ArgumentParser()
p.add_argument('stage', choices=['start', 'capture', 'authorize', 'promote'])
p.add_argument('--work-dir', required=True)
p.add_argument('--image-id', help='owned Linux candidate to authorize without changing the default AMI')
p.add_argument('--base-image-id', help='owned Linux image layer for a new builder; does not change worker defaults')
p.add_argument('--instance-type', default='t3.large', help='disposable builder type; GPU verification needs a GPU builder')
p.add_argument('--prepare-script', type=pathlib.Path, help='additional Linux image recipe, executed only while building')
p.add_argument('--verify-script', type=pathlib.Path, help='Linux acceptance script saved with the build and run before capture')
p.add_argument('--replace-candidate', action='store_true', help='capture an updated builder image; keep the previous image in cleanup inventory')
p.add_argument('--capture-check', type=pathlib.Path, help='additional acceptance script bound before the first capture command; never replaces the build verifier')
a = p.parse_args()
if a.capture_check and a.stage != 'capture':
    p.error('--capture-check applies only to capture')
if bool(a.image_id) != (a.stage == 'authorize'):
    p.error('--image-id is required only for authorize')
if a.stage != 'start' and (a.base_image_id or a.prepare_script or a.verify_script or a.instance_type != 't3.large'):
    p.error('recipe and instance-type options apply only to start')
if bool(a.prepare_script) != bool(a.verify_script):
    p.error('additional preparation requires a matching verification script')
os.umask(0o077)
work = pathlib.Path(a.work_dir).resolve()
path = work / 'resources.json'
s = json.loads(path.read_text())
def save():
    path.write_text(json.dumps(s, indent=2) + '\n')
def aws(service, operation, **inputs):
    r = subprocess.run(['aws', '--region', s['region'], service, operation,
        '--cli-input-json', json.dumps(inputs), '--output', 'json'], capture_output=True, text=True)
    if r.returncode:
        raise RuntimeError(f'{service}/{operation} failed: ' + r.stderr.strip())
    return json.loads(r.stdout) if r.stdout.strip() else {}
if aws('sts', 'get-caller-identity')['Account'] != s['account']:
    raise RuntimeError('AWS account mismatch')
if s.get('cleanedUp'):
    raise RuntimeError('test infrastructure has been cleaned up')
tags = [{'Key': 'cloud-provisioning-test', 'Value': s['runID']}]
def specifications(*kinds):
    return [{'ResourceType': kind, 'Tags': tags} for kind in kinds]
if a.stage == 'start':
    if s.get('builderInstanceID'):
        previous_id = s['builderInstanceID']
        try:
            response = aws('ec2', 'describe-instances', InstanceIds=[previous_id])
        except RuntimeError as error:
            if 'InvalidInstanceID.NotFound' not in str(error):
                raise
            response = {'Reservations': []}
        previous = [i for r in response['Reservations'] for i in r['Instances']]
        if len(previous) > 1 or (previous and previous[0]['InstanceId'] != previous_id):
            raise RuntimeError('ambiguous recorded image builder observation')
        previous_state = previous[0]['State']['Name'] if previous else 'not-found'
        if previous_state in ['terminated', 'not-found']:
            s.setdefault('imageBuilderHistory', []).append(dict(instanceID=previous_id, observedState=previous_state))
            del s['builderInstanceID']
            for key in ['builderVerification', 'builderCommandID', 'builderParent', 'imageRecipe', 'captureCheck']:
                if key in s:
                    s['imageBuilderHistory'][-1][key] = s.pop(key)
            save()
    if s.get('builderInstanceID'):
        if a.base_image_id and s.get('builderParent', {}).get('imageID') != a.base_image_id:
            raise RuntimeError('Recorded builder has a different parent; inspect the original build')
        print('Image builder already recorded:', s['builderInstanceID'])
    else:
        parent = (linux_builder_parent(aws, s, a.base_image_id) if a.base_image_id else
                  dict(imageID=s['baseAMIID'], rootDeviceName='/dev/sda1', rootVolumeGiB=20))
        script = pathlib.Path(__file__).with_name('image-prepare.sh').read_text()
        # Clean only after cloud-final finishes, through a separate SSM command.
        script = script.replace('cloud-init clean --logs --machine-id --seed\n', '')
        if a.prepare_script:
            extra = a.prepare_script.read_text()
            verify = a.verify_script.read_text()
            validate_shell(extra)
            validate_shell(verify)
            (work / 'additional-prepare.sh').write_text(extra)
            (work / 'additional-verify.sh').write_text(verify)
            s['imageRecipe'] = {'prepareSHA256': hashlib.sha256(extra.encode()).hexdigest(),
                'verifySHA256': hashlib.sha256(verify.encode()).hexdigest()}
            script += '\n' + extra + '\n'
        else:
            # A fresh builder without recipe options uses bootstrap only. Keep
            # prior script files as evidence, but never execute their verifier.
            s.pop('imageRecipe', None)
        validate_shell(script)
        s['builderInstanceType'] = a.instance_type
        s['builderParent'] = parent
        save()
        script = '#!/bin/bash\n' + script + '\ntouch /var/lib/cloud-provisioning-image-ready\n'
        if len(script.encode()) > 16384:
            raise RuntimeError('image preparation exceeds EC2 userdata limit; stage artifacts privately')
        result = aws('ec2', 'run-instances', ImageId=parent['imageID'], InstanceType=a.instance_type,
            MinCount=1, MaxCount=1, SubnetId=s['subnetID'], SecurityGroupIds=[s['securityGroupID']],
            IamInstanceProfile={'Name': s['instanceProfileName']},
            MetadataOptions={'HttpTokens': 'required'},
            TagSpecifications=specifications('instance', 'volume', 'network-interface'),
            # AWS CLI's RunInstances handler base64-encodes UserData, even
            # with --cli-input-json. Supplying base64 here encodes it twice.
            UserData=script,
            BlockDeviceMappings=[{'DeviceName': parent['rootDeviceName'], 'Ebs': {'VolumeSize': parent['rootVolumeGiB'],
                'VolumeType': 'gp3', 'Encrypted': True, 'DeleteOnTermination': True}}])
        s['builderInstanceID'] = result['Instances'][0]['InstanceId']
        save()
        print('Started disposable image builder:', s['builderInstanceID'])
elif a.stage == 'capture':
    if a.capture_check and s.get('candidateAMIID') and not a.replace_candidate:
        raise RuntimeError('A candidate already exists; select replacement capture explicitly')
    if s.get('candidateAMIID') and not a.replace_candidate:
        print('Candidate image already recorded:', s['candidateAMIID'])
    else:
        instance = aws('ec2', 'describe-instances', InstanceIds=[s['builderInstanceID']])['Reservations'][0]['Instances'][0]
        if instance['VpcId'] != s['vpcID'] or {t['Key']: t['Value'] for t in instance['Tags']}.get('cloud-provisioning-test') != s['runID']:
            raise RuntimeError('image builder ownership mismatch')
        if s.get('builderParent') and instance.get('ImageId') != s['builderParent']['imageID']:
            raise RuntimeError('image builder parent differs from the recorded launch')
        # Capture validates the build; it must not download/reinstall tooling.
        prepare = "set -eu\n/usr/local/bin/aws --version\npython3 -c 'from cloudinit import features; assert not features.ERROR_ON_USER_DATA_FAILURE'\n"
        if s.get('imageRecipe'):
            verify = (work / 'additional-verify.sh').read_text()
            if hashlib.sha256(verify.encode()).hexdigest() != s['imageRecipe']['verifySHA256']:
                raise RuntimeError('saved image verification script has changed')
            prepare += verify + '\n'
        if a.capture_check:
            extra_check = a.capture_check.read_text()
            validate_shell(extra_check)
            digest = hashlib.sha256(extra_check.encode()).hexdigest()
            binding = dict(instanceID=s['builderInstanceID'], sha256=digest,
                           file='capture-check-'+digest+'.sh')
            if s.get('captureCheck') and s['captureCheck'] != binding:
                raise RuntimeError('A different capture check is already bound; inspect the original build')
            if not s.get('captureCheck'):
                if s.get('builderVerification') or s.get('builderCommandID'):
                    raise RuntimeError('Capture has already begun; cannot add acceptance to its original command')
                target = work/binding['file']
                if target.exists():
                    if target.read_text() != extra_check:
                        raise RuntimeError('Saved capture check content differs')
                else:
                    with target.open('x') as stream:
                        stream.write(extra_check)
                s['captureCheck'] = binding
                save()
        if s.get('captureCheck'):
            binding = s['captureCheck']
            extra_check = (work/binding['file']).read_text()
            if (binding['instanceID'] != s['builderInstanceID'] or
                    hashlib.sha256(extra_check.encode()).hexdigest() != binding['sha256']):
                raise RuntimeError('Saved capture check identity or content changed')
            prepare += extra_check + '\n'
        prepare += 'cloud-init clean --logs --machine-id --seed\nsync\n'
        command = ('set -eu; test -f /var/lib/cloud-provisioning-image-ready; '
            'if test -e /var/lib/cloud/data/status.json; then cloud-init status --wait; fi; cloud-init --version; '
            'test ! -e /etc/secret-userdata.txt; bash -c ' + shlex.quote(prepare))
        if a.replace_candidate and s.get('builderVerification', {}).get('capturedImageID'):
            if s['builderVerification']['capturedImageID'] != s.get('candidateAMIID'):
                raise RuntimeError('recorded verification does not match the candidate being replaced')
            s.setdefault('builderVerificationHistory', []).append(s.pop('builderVerification'))
            s.pop('builderCommandID', None)
            save()
        identity = dict(instanceID=s['builderInstanceID'],
                        commandSHA256=hashlib.sha256(command.encode()).hexdigest())
        verification = s.get('builderVerification')
        if verification is not None:
            if any(verification.get(k) != v for k, v in identity.items()):
                raise RuntimeError('recorded builder verification belongs to a different instance or recipe')
            if not verification.get('commandID'):
                raise RuntimeError('SSM submission outcome is unknown; reconcile the recorded intent before capture')
        else:
            if s.get('builderCommandID'):
                raise RuntimeError('legacy builder command has no instance/recipe binding; reconcile it before capture')
            # Persist intent first. A lost send-command response must not cause a
            # second cloud-init cleanup while the original guest process runs.
            verification = dict(identity, status='submission-pending')
            s['builderVerification'] = verification
            save()
            sent = aws('ssm', 'send-command', InstanceIds=[s['builderInstanceID']],
                DocumentName='AWS-RunShellScript', Parameters={'commands': [command], 'executionTimeout': ['600']})
            verification.update(commandID=sent['Command']['CommandId'], status='submitted')
            s['builderCommandID'] = verification['commandID']
            save()
        command_id = verification['commandID']
        if verification['status'] != 'Success' or verification.get('responseCode') != 0:
            deadline = time.monotonic() + 630
            while time.monotonic() < deadline:
                time.sleep(3)
                try:
                    result = aws('ssm', 'get-command-invocation', InstanceId=s['builderInstanceID'], CommandId=command_id)
                except RuntimeError as err:
                    if 'InvocationDoesNotExist' in str(err):
                        continue
                    raise
                if result['Status'] in ['Pending', 'InProgress', 'Delayed', 'Cancelling']:
                    continue
                (work / 'image-preparation.json').write_text(json.dumps(result, indent=2) + '\n')
                verification['responseCode'] = result['ResponseCode']
                if result['Status'] != 'Success' or result['ResponseCode'] != 0:
                    verification['status'] = result['Status']
                    save()
                    raise RuntimeError('original image preparation did not succeed; inspect image-preparation.json')
                verification['status'] = 'Success'
                save()
                break
            else:
                raise RuntimeError('observation timed out; rerun capture to observe the original SSM command')
        # Snapshot only a stopped builder. NoReboot on a running guest can
        # capture directory entries before their dirty file contents reach EBS.
        aws('ec2', 'stop-instances', InstanceIds=[s['builderInstanceID']])
        deadline = time.monotonic() + 300
        while time.monotonic() < deadline:
            instance = aws('ec2', 'describe-instances', InstanceIds=[s['builderInstanceID']])['Reservations'][0]['Instances'][0]
            if instance['State']['Name'] == 'stopped':
                break
            time.sleep(5)
        else:
            raise RuntimeError('builder has not stopped; no image captured')
        image = aws('ec2', 'create-image', InstanceId=s['builderInstanceID'], NoReboot=True,
            Name=s['runID']+'-capa-'+str(int(time.time())), TagSpecifications=specifications('image', 'snapshot'))
        s['candidateAMIID'] = image['ImageId']
        verification['capturedImageID'] = image['ImageId']
        if s.get('builderParent'):
            verification['parent'] = s['builderParent']
        # Record immediately so cleanup owns the image even before promotion.
        s.setdefault('retiredImages', []).append({'id': image['ImageId'], 'snapshots': []})
        save()
        print('Captured candidate image:', image['ImageId'])
elif a.stage == 'authorize':
    owned_image(aws, s, a.image_id, 'linux')
    role = aws('iam', 'get-role', RoleName=s['capaRoleName'])['Role']
    if role['Arn'] != s['capaRoleARN'] or {t['Key']: t['Value'] for t in role.get('Tags', [])}.get('cloud-provisioning-test') != s['runID']:
        raise RuntimeError('CAPA role ownership mismatch')
    # Extend the live policy rather than reconstructing it from one OS default.
    policy = aws('iam', 'get-role-policy', RoleName=s['capaRoleName'], PolicyName='cldt-test')['PolicyDocument']
    launch = [item for item in policy['Statement'] if item.get('Sid') == 'UseApprovedLaunchResources']
    if len(launch) != 1 or launch[0].get('Effect') != 'Allow' or launch[0].get('Action') != ['ec2:RunInstances'] or not isinstance(launch[0].get('Resource'), list):
        raise RuntimeError('unexpected CAPA launch policy shape')
    arn = 'arn:aws:ec2:'+s['region']+'::image/'+a.image_id
    launch[0]['Resource'] = list(dict.fromkeys(launch[0]['Resource']+[arn]))
    aws('iam', 'put-role-policy', RoleName=s['capaRoleName'], PolicyName='cldt-test', PolicyDocument=json.dumps(policy))
    (work/'capa-policy.json').write_text(json.dumps(policy, indent=2)+'\n')
    s['authorizedLinuxAMIs'] = list(dict.fromkeys(s.get('authorizedLinuxAMIs', [])+[a.image_id]))
    save()
    print('Authorized owned Linux candidate; default image and existing policy grants retained:', a.image_id)
else:
    image = owned_image(aws, s, s['candidateAMIID'], 'linux')
    # Linux and Windows claims share this role. Re-rendering from the Linux
    # AMI alone would silently revoke the already approved Windows templates.
    windows_images = []
    for image_id in s.get('authorizedWindowsAMIs', []):
        owned_image(aws, s, image_id, 'windows')
        windows_images.append('arn:aws:ec2:'+s['region']+'::image/'+image_id)
    role = aws('iam', 'get-role', RoleName=s['capaRoleName'])['Role']
    if role['Arn'] != s['capaRoleARN'] or {t['Key']: t['Value'] for t in role.get('Tags', [])}.get('cloud-provisioning-test') != s['runID']:
        raise RuntimeError('CAPA role ownership mismatch')
    linux_images = retained_linux_arns(aws, s)
    snapshots = [m['Ebs']['SnapshotId'] for m in image['BlockDeviceMappings'] if 'Ebs' in m]
    values_path = work / 'iam-values.json'
    values = json.loads(values_path.read_text())
    values['AMI_ID'] = image['ImageId']
    values_path.write_text(json.dumps(values, indent=2) + '\n')
    iam = pathlib.Path(__file__).resolve().parents[3] / 'docs' / 'iam'
    policy = subprocess.check_output(['python3', str(iam/'render.py'), str(iam/'capa.json'), str(values_path)], text=True)
    document = json.loads(policy)
    launch = next(item for item in document['Statement'] if item.get('Sid') == 'UseApprovedLaunchResources')
    launch['Resource'] = list(dict.fromkeys(launch['Resource'] + windows_images + linux_images))
    policy = json.dumps(document, indent=2)+'\n'
    aws('iam', 'put-role-policy', RoleName=s['capaRoleName'], PolicyName='cldt-test', PolicyDocument=policy)
    (work/'capa-policy.json').write_text(policy)
    s['amiID'] = s['preparedAMIID'] = image['ImageId']
    s['preparedSnapshotIDs'] = snapshots
    for item in s['retiredImages']:
        if item['id'] == image['ImageId']:
            item['snapshots'] = snapshots
    save()
    aws('ec2', 'terminate-instances', InstanceIds=[s['builderInstanceID']])
    print('Approved worker AMI and terminated disposable builder:', image['ImageId'])
