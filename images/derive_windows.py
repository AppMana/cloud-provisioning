#!/usr/bin/env python3
"""Derive a Windows image by replacing one executable in a pinned base."""
import argparse
import hashlib
import io
import json
import pathlib
import re
import struct
import subprocess
import tarfile
import tempfile


def checked_binary(path, expected):
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != expected:
        raise ValueError('binary SHA-256 differs from the requested build')
    if len(raw) < 64 or raw[:2] != b'MZ':
        raise ValueError('not a Windows executable')
    offset = struct.unpack_from('<I', raw, 60)[0]
    if offset + 6 > len(raw) or raw[offset:offset+4] != b'PE\0\0' or struct.unpack_from('<H', raw, offset+4)[0] != 0x8664:
        raise ValueError('require an amd64 PE executable')
    return raw


def run(*args):
    return subprocess.check_output(args, text=True, timeout=600).strip()


def write_binary_layer(layer, raw, executable):
    path = pathlib.PurePosixPath(executable)
    if path.is_absolute() or any(x in executable for x in ("\\", ":")) or any(x in ("", ".", "..") for x in executable.split("/")):
        raise ValueError("require a relative executable path without traversal")
    with tarfile.open(layer, 'w', format=tarfile.PAX_FORMAT) as tar:
        # Windows extracts each layer independently: a parent in the base
        # layer does not satisfy file creation in this layer's staging tree.
        for parent in reversed(path.parents):
            if str(parent) == '.':
                continue
            directory = tarfile.TarInfo(str(parent)+'/')
            directory.type = tarfile.DIRTYPE
            directory.mode = 0o755
            tar.addfile(directory)
        info = tarfile.TarInfo(str(path))
        info.size = len(raw)
        info.mode = 0o644
        tar.addfile(info, io.BytesIO(raw))


def build(base, binary, expected, output, executable, component):
    if not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', base):
        raise ValueError('base must pin a single-platform image by SHA-256 digest')
    raw = checked_binary(binary, expected)
    config = json.loads(run('crane', 'config', base))
    manifest = json.loads(run('crane', 'manifest', base))
    if config.get('os') != 'windows' or config.get('architecture') != 'amd64' or 'layers' not in manifest:
        raise ValueError('base must be a Windows amd64 image, not an index')
    output.mkdir(parents=True, exist_ok=False)
    archive = output/'image.tar'
    with tempfile.TemporaryDirectory(dir=output, prefix='layer-') as temp:
        layer = pathlib.Path(temp)/'layer.tar'
        write_binary_layer(layer, raw, executable)
        run('crane', 'append', '--base', base, '--new_layer', str(layer),
            '--new_tag', 'cldt/'+component+':'+expected[:16], '--output', str(archive))
    # Inspect the actual archive, including crane's Windows layer conversion.
    with tarfile.open(archive) as tar:
        entries = json.load(tar.extractfile('manifest.json'))
        if len(entries) != 1:
            raise ValueError('expected one resulting image')
        entry = entries[0]
        actual = json.load(tar.extractfile(entry['Config']))
        for key in ('os', 'architecture', 'os.version', 'config'):
            if actual.get(key) != config.get(key):
                raise ValueError('image derivation changed base configuration: '+key)
        if actual['rootfs']['diff_ids'][:-1] != config['rootfs']['diff_ids'] or len(entry['Layers']) != len(manifest['layers'])+1:
            raise ValueError('image derivation did not preserve all base layers')
        with tarfile.open(fileobj=tar.extractfile(entry['Layers'][-1]), mode='r:*') as layer:
            for parent in pathlib.PurePosixPath(executable).parents:
                if str(parent) != '.' and not any(m.isdir() and m.name.rstrip('/') == 'Files/'+str(parent) for m in layer.getmembers()):
                    raise ValueError('Windows layer omits executable parent directory')
            candidates = [m for m in layer.getmembers() if m.isfile() and m.name.rstrip('/') == 'Files/'+executable]
            if len(candidates) != 1 or hashlib.sha256(layer.extractfile(candidates[0]).read()).hexdigest() != expected:
                raise ValueError('Windows layer does not contain the expected executable')
    digest = run('crane', 'digest', '--tarball', str(archive))
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
        raise ValueError('invalid resulting image digest')
    with archive.open('rb') as body:
        archive_hash = hashlib.file_digest(body, 'sha256').hexdigest()
    receipt = dict(baseImage=base, imageDigest=digest, binarySHA256=expected,
                   platform={k:config[k] for k in ('os','architecture','os.version') if k in config},
                   baseLayersPreserved=True, startupConfigurationPreserved=True,
                   replacedFile='Files/'+executable,
                   imageArchiveSHA256=archive_hash)
    (output/'build.json').write_text(json.dumps(receipt, indent=2)+'\n')
    return receipt


def main(executable, component):
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base', required=True)
    p.add_argument('--binary', required=True, type=pathlib.Path)
    p.add_argument('--sha256', required=True)
    p.add_argument('--output', required=True, type=pathlib.Path)
    a = p.parse_args()
    print(json.dumps(build(a.base, a.binary, a.sha256, a.output, executable, component)))
