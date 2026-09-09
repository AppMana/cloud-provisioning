#!/usr/bin/env python3
"""Bake a verified k0s executable into an unjoined Linux image.

Stage the artifact through the provider's build transport first. This step has
no download, cloud-agent, driver, CNI, service-installation or joining behavior.
"""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import subprocess
import tempfile


def prepare(artifact, sha256, version, root=pathlib.Path('/'), run=subprocess.run):
    if not re.fullmatch('[a-f0-9]{64}', sha256):
        raise ValueError('Require the artifact SHA256')
    if not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+\+k0s\.[0-9]+', version):
        raise ValueError('Require an exact k0s release')
    for directory in ['etc/k0s', 'var/lib/k0s']:
        path = root/directory
        if path.is_symlink() or (path.exists() and (not path.is_dir() or any(path.iterdir()))):
            raise ValueError('Image already contains k0s configuration or runtime state')
    for service in ['k0sworker', 'k0scontroller']:
        result = run(['systemctl', 'show', service, '--property=LoadState', '--value'],
                     capture_output=True, text=True, check=True)
        if result.stdout.strip() != 'not-found':
            raise ValueError('Image already has a k0s service')
    data = pathlib.Path(artifact).read_bytes()
    if hashlib.sha256(data).hexdigest() != sha256:
        raise ValueError('Artifact checksum mismatch')
    target = root/'usr/local/bin/k0s'
    receipt = root/'etc/cloud-provisioning-image/k0s.json'
    expected = dict(schemaVersion=1, version=version, sha256=sha256,
                    executable='/usr/local/bin/k0s', scope='Executable only; no runtime or workload cache')
    if target.is_symlink() or (target.exists() and hashlib.sha256(target.read_bytes()).hexdigest() != sha256):
        raise ValueError('Refuse to replace a different installed k0s artifact')
    if receipt.exists():
        recorded = json.loads(receipt.read_text())
        if not target.exists() or any(recorded.get(k) != v for k, v in expected.items()):
            raise ValueError('Existing preparation receipt does not match the image')
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix='.k0s-image-', dir=target.parent)
    temporary = pathlib.Path(temporary)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(data)
            stream.flush()
            os.fchmod(stream.fileno(), 0o755)
            os.fsync(stream.fileno())
        result = run([str(temporary), 'version'], capture_output=True, text=True, check=True)
        if result.stdout.strip() != version:
            raise ValueError('Native executable version differs from the requested release')
        if not target.exists():
            os.replace(temporary, target)
        if not receipt.exists():
            receipt.parent.mkdir(parents=True, exist_ok=True)
            with receipt.open('x') as stream:
                json.dump(dict(expected, preparedAt=datetime.datetime.now(datetime.timezone.utc).isoformat()), stream, indent=2)
                stream.write('\n')
        return json.loads(receipt.read_text())
    finally:
        temporary.unlink(missing_ok=True)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--artifact', type=pathlib.Path, required=True)
    p.add_argument('--sha256', required=True)
    p.add_argument('--version', required=True)
    a = p.parse_args()
    if os.geteuid() != 0 or os.uname().sysname != 'Linux':
        p.error('Run as root in the unjoined Linux image builder')
    print(json.dumps(prepare(a.artifact, a.sha256, a.version)))


if __name__ == '__main__':
    main()
