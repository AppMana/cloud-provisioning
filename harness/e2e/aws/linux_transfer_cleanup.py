"""Remove explicitly verified, completed SSM transfer scripts before image capture.

The AWS adapter must obtain terminal GetCommandInvocation receipts first.
This helper leaves agent identity, active commands and other history intact.
"""
import hashlib
import pathlib
import re

INSTANCE = re.compile(r'i-[a-f0-9]{8,17}')
COMMAND = re.compile(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}')
SIGNED_URL = re.compile(rb'X-Amz-Signature=[a-fA-F0-9]{64}')


def cleanup(manifest, ssm_root=pathlib.Path('/var/lib/amazon/ssm'), proc=pathlib.Path('/proc')):
    instance = manifest['instanceID']
    if not INSTANCE.fullmatch(instance) or not manifest['commands']:
        raise ValueError('Require an instance and explicit completed commands')
    root = ssm_root/instance/'document'
    planned = []
    seen = set()
    for command in manifest['commands']:
        identifier = command['CommandId']
        if (not COMMAND.fullmatch(identifier) or identifier in seen or
                command['InstanceId'] != instance or command['Status'] != 'Success' or
                type(command['ResponseCode']) is not int or command['ResponseCode'] != 0):
            raise ValueError('Require unique successful transfer receipts for this instance')
        seen.add(identifier)
        relative = f'orchestration/{identifier}/awsrunShellScript/0.awsrunShellScript/_script.sh'
        if command['path'] != relative:
            raise ValueError('Receipt path does not match the original command')
        path = root/relative
        if any(parent.is_symlink() for parent in [path, *path.parents]):
            raise ValueError('Refuse symbolic links in SSM history')
        data = path.read_bytes()
        if not SIGNED_URL.search(data):
            raise ValueError('Original script has no signed transfer URL; retain it')
        planned.append((path, hashlib.sha256(data).hexdigest(), path.stat().st_ino))
    # A successful service receipt does not justify unlinking a script that
    # a guest process still references. Inspect all processes before any write.
    for cmdline in proc.glob('[0-9]*/cmdline'):
        try:
            arguments = cmdline.read_bytes()
        except FileNotFoundError:
            continue
        if any(str(path).encode() in arguments for path, _, _ in planned):
            raise ValueError('A guest process still references a selected transfer script')
    for path, digest, inode in planned:
        if path.stat().st_ino != inode or hashlib.sha256(path.read_bytes()).hexdigest() != digest:
            raise ValueError('Transfer history changed during inspection')
    removed = []
    for path, digest, _ in planned:
        path.unlink()
        removed.append(dict(path=str(path.relative_to(root)), sha256=digest))
    return dict(instanceID=instance, removed=removed, scope='Explicit completed transfer scripts only')
