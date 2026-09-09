"""Shared image ownership checks for mixed Linux/Windows CAPA launch policies."""


def owned_image(aws, state, image_id, machine_os):
    images = aws('ec2', 'describe-images', ImageIds=[image_id])['Images']
    if len(images) != 1:
        raise RuntimeError('expected exactly one recorded image')
    image = images[0]
    actual_os = 'windows' if image.get('Platform') == 'windows' else 'linux'
    if (image['ImageId'] != image_id or image['State'] != 'available' or image['Public']
            or image['OwnerId'] != state['account'] or actual_os != machine_os
            or {t['Key']: t['Value'] for t in image.get('Tags', [])}.get('cloud-provisioning-test') != state['runID']):
        raise RuntimeError('image ownership, operating system or availability mismatch')
    return image


def retained_linux_arns(aws, state):
    ids = list(dict.fromkeys(state.get('authorizedLinuxAMIs', [])))
    for image_id in ids:
        owned_image(aws, state, image_id, 'linux')
    return ['arn:aws:ec2:'+state['region']+'::image/'+image_id for image_id in ids]


def linux_builder_parent(aws, state, image_id):
    """Select an owned reusable layer without promoting it for worker launch."""
    image = owned_image(aws, state, image_id, 'linux')
    if (image.get('Architecture') != 'x86_64' or image.get('VirtualizationType') != 'hvm'
            or image.get('RootDeviceType') != 'ebs'):
        raise RuntimeError('Linux builder requires an x86_64 HVM EBS parent image')
    disks = [disk for disk in image.get('BlockDeviceMappings', []) if 'Ebs' in disk]
    if len(disks) != 1 or disks[0].get('DeviceName') != image.get('RootDeviceName'):
        raise RuntimeError('Linux builder requires exactly one root EBS volume')
    size = disks[0]['Ebs'].get('VolumeSize')
    if type(size) is not int or size < 1:
        raise RuntimeError('Parent image root volume size is missing or invalid')
    return dict(imageID=image_id, rootDeviceName=image['RootDeviceName'],
                rootVolumeGiB=max(20, size))
