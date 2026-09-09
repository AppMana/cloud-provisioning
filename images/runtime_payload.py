#!/usr/bin/env python3
"""Extract OS-specific runtime inputs from a hash-pinned k0s worker.

This prepares inputs for image building. It neither starts a runtime nor proves
that a container image has been unpacked or survives cloning an OS image.
"""
import argparse
import hashlib
import json
import pathlib
import re
import zipfile

PROFILES = {
    'windows': (('containerd.exe', 'containerd-shim-runhcs-v1.exe'), 0o100644),
    'linux': (('containerd', 'containerd-shim-runc-v2', 'runc'), 0o100755),
}


def stage_runtime(worker, expected, output, *, machine_os):
    if machine_os not in PROFILES:
        raise ValueError('unsupported runtime operating system')
    components, file_mode = PROFILES[machine_os]
    if not re.fullmatch('[0-9a-f]{64}', expected):
        raise ValueError('require the expected worker SHA-256')
    with worker.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != expected:
            raise ValueError('worker SHA-256 mismatch')
    payload = {}
    with zipfile.ZipFile(worker) as archive:
        for name in components:
            entries = [entry for entry in archive.infolist() if entry.filename == name]
            if len(entries) != 1 or entries[0].is_dir() or not 0 < entries[0].file_size <= 256 << 20:
                raise ValueError('require exactly one bounded runtime component: '+name)
            payload[name] = archive.read(entries[0])
    # A conventional ZIP starts at offset zero, unlike k0s's appended payload
    # which the tested .NET ZIP reader could not parse on either Windows OS.
    output.mkdir(parents=True, exist_ok=False)
    archive_path = output/'runtime.zip'
    with zipfile.ZipFile(archive_path, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
        for name, body in payload.items():
            entry = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            entry.compress_type = zipfile.ZIP_DEFLATED
            entry.external_attr = file_mode << 16
            archive.writestr(entry, body)
    with archive_path.open('rb') as stream:
        archive_hash = hashlib.file_digest(stream, 'sha256').hexdigest()
    receipt = {'schemaVersion': 1, 'workerSHA256': expected, 'runtimeArchiveSHA256': archive_hash,
               'components': [{'name': name, 'bytes': len(body), 'sha256': hashlib.sha256(body).hexdigest()}
                              for name, body in payload.items()],
               'scope': 'Exact embedded runtime bytes only; image cache and clone startup not qualified'}
    (output/'runtime.json').write_text(json.dumps(receipt, indent=2)+'\n')
    return receipt


def main(machine_os):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--worker', type=pathlib.Path, required=True)
    parser.add_argument('--sha256', required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    print(json.dumps(stage_runtime(args.worker, args.sha256, args.output, machine_os=machine_os)))
