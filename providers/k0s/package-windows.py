#!/usr/bin/env python3
"""Attach the verified upstream k0s Windows runtime payload to a rebuilt worker."""
import argparse
import hashlib
import io
import json
import pathlib
import struct
import zipfile


def verified_pe(path, expected):
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != expected:
        raise ValueError('executable checksum mismatch')
    if len(data) < 64 or data[:2] != b'MZ':
        raise ValueError('expected a PE executable')
    offset = struct.unpack_from('<I', data, 60)[0]
    if offset + 6 > len(data) or data[offset:offset+4] != b'PE\0\0' or struct.unpack_from('<H', data, offset+4)[0] != 0x8664:
        raise ValueError('expected an amd64 PE executable')
    return data


def package(bare, bare_sha256, upstream, upstream_sha256, output):
    worker = verified_pe(bare, bare_sha256)
    original = verified_pe(upstream, upstream_sha256)
    if zipfile.is_zipfile(io.BytesIO(worker)):
        raise ValueError('rebuilt worker already has an attached ZIP payload')
    with zipfile.ZipFile(io.BytesIO(original)) as archive:
        entries = archive.infolist()
        names = [entry.filename for entry in entries]
        required = {'containerd.exe', 'containerd-shim-runhcs-v1.exe', 'kubelet.exe'}
        if set(names) != required or len(names) != len(required):
            raise ValueError('unexpected Windows runtime payload inventory')
        hashes = {entry.filename: hashlib.sha256(archive.read(entry)).hexdigest() for entry in entries}
        # k0s appends a ZIP to its executable. Preserve that ZIP byte for byte,
        # including timestamps, compression and runtime executables.
        payload = original[min(entry.header_offset for entry in entries):]
    combined = worker + payload
    with zipfile.ZipFile(io.BytesIO(combined)) as archive:
        actual = {entry.filename: hashlib.sha256(archive.read(entry)).hexdigest() for entry in archive.infolist()}
        if actual != hashes:
            raise ValueError('packaging changed embedded runtimes')
    receipt = dict(bareSHA256=bare_sha256, upstreamSHA256=upstream_sha256,
                   payloadSHA256=hashlib.sha256(payload).hexdigest(),
                   binarySHA256=hashlib.sha256(combined).hexdigest(), embeddedSHA256=hashes)
    output.mkdir(parents=True, exist_ok=False)
    (output / 'k0s.exe').write_bytes(combined)
    (output / 'package.json').write_text(json.dumps(receipt, indent=2) + '\n')
    return receipt


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('bare', 'upstream', 'output'):
        parser.add_argument('--' + name, type=pathlib.Path, required=True)
    for name in ('bare-sha256', 'upstream-sha256'):
        parser.add_argument('--' + name, required=True)
    args = parser.parse_args()
    print(json.dumps(package(args.bare, args.bare_sha256, args.upstream,
                             args.upstream_sha256, args.output)))
