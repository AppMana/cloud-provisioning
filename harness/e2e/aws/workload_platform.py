"""Validate an ordinary Windows workload's declared build before publication."""
import hashlib
import json
import re


def layout_platform(layout, year):
    def read(digest):
        if not re.fullmatch('sha256:[a-f0-9]{64}', digest):
            raise RuntimeError('Require a SHA256-addressed OCI object')
        raw = (layout / 'blobs' / 'sha256' / digest.split(':')[1]).read_bytes()
        if hashlib.sha256(raw).hexdigest() != digest.split(':')[1]:
            raise RuntimeError('OCI object digest mismatch')
        return json.loads(raw)
    index = json.loads((layout / 'index.json').read_text())
    if len(index['manifests']) != 1:
        raise RuntimeError('Require exactly one workload platform')
    descriptor = index['manifests'][0]
    manifest = read(descriptor['digest'])
    config = read(manifest['config']['digest'])
    platform = {key: config.get(key) for key in ['os', 'architecture', 'os.version']}
    build = {'2022': '20348', '2025': '26100'}[year]
    if (platform['os'] != 'windows' or platform['architecture'] != 'amd64'
            or not re.fullmatch(r'10\.0\.' + build + r'\.\d+', platform['os.version'] or '')
            or any(descriptor.get('platform', {}).get(key) != value for key, value in platform.items())):
        raise RuntimeError('Workload OCI platform does not match the selected Windows release')
    return platform
