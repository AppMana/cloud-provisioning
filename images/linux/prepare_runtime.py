#!/usr/bin/env python3
"""Stage k0s's embedded Linux runtime in an unjoined image; no runtime is started."""
import argparse
import datetime
import hashlib
import json
import os
import pathlib
import re
import stat
import subprocess
import tempfile
import zipfile

COMPONENTS = ('containerd', 'containerd-shim-runc-v2', 'runc')


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def publish(path, body, mode, mtime_ns=None):
    fd, temporary = tempfile.mkstemp(prefix='.'+path.name+'-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(body)
            os.fchmod(stream.fileno(), mode)
            stream.flush()
            os.fsync(stream.fileno())
        if mtime_ns is not None:
            os.utime(temporary, ns=(mtime_ns, mtime_ns))
        # Atomic creation without replacing a file belonging to another operation.
        os.link(temporary, path)
    finally:
        os.unlink(temporary)


def payload(archive, name):
    entries = [entry for entry in archive.infolist() if entry.filename == name]
    if len(entries) != 1 or entries[0].is_dir() or not 0 < entries[0].file_size <= 256 << 20:
        raise ValueError('Require exactly one bounded runtime component: '+name)
    return archive.read(entries[0])


def prepare(runtime, manifest_path, root=pathlib.Path('/'), run=subprocess.run):
    metadata = root/'etc/cloud-provisioning-image'
    worker = root/'usr/local/bin/k0s'
    data = root/'var/lib/k0s'
    binaries = data/'bin'
    receipt_path = metadata/'linux-runtime.json'
    intent = metadata/'linux-runtime.intent.json'
    for path in [metadata, worker, data, binaries, receipt_path, intent, root/'etc/k0s', root/'etc/kubernetes']:
        if path.is_symlink():
            raise ValueError('Image preparation paths must not be symlinks')
    for path in [root/'etc/k0s', root/'etc/kubernetes']:
        if path.exists() and (not path.is_dir() or any(path.iterdir())):
            raise ValueError('Image contains cluster configuration')
    for service in ['k0sworker', 'k0scontroller', 'kubelet']:
        result = run(['systemctl', 'show', service, '--property=LoadState', '--value'],
                     capture_output=True, text=True, check=True)
        if result.stdout.strip() != 'not-found':
            raise ValueError('Image has a Kubernetes service')
    if data.exists() and (not data.is_dir() or any(x.name != 'bin' for x in data.iterdir())):
        raise ValueError('Image contains runtime or cluster state; stage binaries before cache preparation')
    manifest = json.loads(manifest_path.read_text())
    worker_receipt = json.loads((metadata/'k0s.json').read_text())
    worker_hash = digest(worker)
    if manifest.get('schemaVersion') != 1 or manifest.get('workerSHA256') != worker_hash or worker_receipt.get('sha256') != worker_hash:
        raise ValueError('Runtime manifest and prepared k0s identity differ')
    if digest(runtime) != manifest.get('runtimeArchiveSHA256'):
        raise ValueError('Runtime archive SHA256 mismatch')
    components = manifest.get('components', [])
    if not isinstance(components, list) or [x.get('name') for x in components] != list(COMPONENTS):
        raise ValueError('Require the Linux runtime component profile')
    contents = {}
    with zipfile.ZipFile(runtime) as archive, zipfile.ZipFile(worker) as embedded:
        if sorted(archive.namelist()) != sorted(COMPONENTS):
            raise ValueError('Unexpected or duplicate runtime archive entries')
        for component in components:
            name = component['name']
            body = payload(archive, name)
            if (len(body) != component.get('bytes') or hashlib.sha256(body).hexdigest() != component.get('sha256')
                    or body != payload(embedded, name)):
                raise ValueError('Runtime component differs from the embedded k0s payload: '+name)
            contents[name] = body
    mtime_ns = worker.stat().st_mtime_ns
    expected = dict(schemaVersion=1, workerSHA256=worker_hash, workerMtimeNs=mtime_ns,
                    runtimeArchiveSHA256=manifest['runtimeArchiveSHA256'], components=components,
                    binaryDirectory='/var/lib/k0s/bin', fileMode='0750',
                    scope='Runtime executable staging only; no service, runtime store or image cache')
    if receipt_path.exists():
        receipt = json.loads(receipt_path.read_text())
        if any(receipt.get(k) != v for k, v in expected.items()):
            raise ValueError('Existing runtime receipt differs from requested inputs')
        if not binaries.is_dir() or sorted(x.name for x in binaries.iterdir()) != sorted(COMPONENTS):
            raise ValueError('Prepared runtime inventory changed')
        for name, body in contents.items():
            target = binaries/name
            if (target.is_symlink() or not target.is_file() or target.read_bytes() != body
                    or target.stat().st_mtime_ns != mtime_ns or stat.S_IMODE(target.stat().st_mode) != 0o750):
                raise ValueError('Prepared runtime file identity, timestamp or mode changed')
        return receipt
    if intent.exists():
        raise ValueError('Inspect the original incomplete runtime preparation; do not replay it')
    if binaries.exists() and (not binaries.is_dir() or any(binaries.iterdir())):
        raise ValueError('Refuse an unrecorded runtime binary directory')
    publish(intent, (json.dumps(expected, indent=2)+'\n').encode(), 0o600)
    binaries.mkdir(parents=True, exist_ok=True)
    for name, body in contents.items():
        publish(binaries/name, body, 0o750, mtime_ns)
    result = run([str(binaries/'containerd'), '--version'], capture_output=True, text=True, check=True)
    if not re.search(r'^containerd\s+\S+\s+v?2\.\d+\.\d+(?:\s|$)', result.stdout.strip()):
        raise ValueError('Require a native containerd 2 executable')
    receipt = dict(expected, containerdVersion=result.stdout.strip(),
                   preparedAt=datetime.datetime.now(datetime.timezone.utc).isoformat())
    publish(receipt_path, (json.dumps(receipt, indent=2)+'\n').encode(), 0o600)
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime-archive', type=pathlib.Path, required=True)
    parser.add_argument('--runtime-manifest', type=pathlib.Path, required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or os.uname().sysname != 'Linux':
        parser.error('Run as root in an unjoined Linux image builder')
    print(json.dumps(prepare(args.runtime_archive, args.runtime_manifest)))


if __name__ == '__main__':
    main()
