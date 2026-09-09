#!/usr/bin/env python3
"""Replace a verified Windows base while retaining exact application layers.

Produces a local OCI layout. Does not publish, unpack, or run the resulting image.
"""
import argparse
import copy
import hashlib
import json
import pathlib
import re
import subprocess


def command(*args):
    return subprocess.check_output(args, timeout=1800)


def pinned(ref):
    if not re.fullmatch(r'[^\s@]+@sha256:[0-9a-f]{64}', ref):
        raise ValueError('Require single-platform digest-pinned image references')
    return ref


def metadata(ref):
    pinned(ref)
    manifest = json.loads(command('crane', 'manifest', ref))
    config = json.loads(command('crane', 'config', ref))
    if 'layers' not in manifest or config.get('os') != 'windows' or config.get('architecture') != 'amd64':
        raise ValueError('Require a single Windows amd64 image, not an index')
    return manifest, config


def replacement(original, old, new):
    app_manifest, app_config = original
    old_manifest, old_config = old
    new_manifest, new_config = new
    for manifest, config in [original, old, new]:
        if (config.get('os') != 'windows' or config.get('architecture') != 'amd64' or
                not re.fullmatch(r'10\.0\.\d+\.\d+', config.get('os.version', '')) or
                not manifest.get('layers') or config.get('rootfs', {}).get('type') != 'layers' or
                len(manifest['layers']) != len(config['rootfs']['diff_ids'])):
            raise ValueError('Invalid Windows image layer/platform metadata')
    count = len(old_manifest['layers'])
    history_count = len(old_config.get('history', []))
    if (len(app_manifest['layers']) <= count or
            app_manifest['layers'][:count] != old_manifest['layers'] or
            app_config['rootfs']['diff_ids'][:count] != old_config['rootfs']['diff_ids'] or
            app_config.get('history', [])[:history_count] != old_config.get('history', []) or
            app_config['os.version'] != old_config['os.version']):
        raise ValueError('Original does not contain the exact declared old base')
    manifest = copy.deepcopy(app_manifest)
    manifest['layers'] = copy.deepcopy(new_manifest['layers'] + app_manifest['layers'][count:])
    config = copy.deepcopy(app_config)
    for key in ['os', 'architecture', 'os.version', 'os.features', 'variant']:
        config.pop(key, None)
        if key in new_config:
            config[key] = copy.deepcopy(new_config[key])
    config['rootfs'] = {'type': 'layers', 'diff_ids': new_config['rootfs']['diff_ids'] + app_config['rootfs']['diff_ids'][count:]}
    config['history'] = new_config.get('history', []) + app_config.get('history', [])[history_count:]
    config['created'] = new_config.get('created', app_config.get('created'))
    return manifest, config, app_manifest['layers'][count:]


def blob(layout, value):
    raw = json.dumps(value, separators=(',', ':')).encode()
    digest = hashlib.sha256(raw).hexdigest()
    (layout/'blobs'/'sha256'/digest).write_bytes(raw)
    return {'digest': 'sha256:'+digest, 'size': len(raw)}


def build(original, old_base, new_base, output):
    app, old, new = [metadata(ref) for ref in [original, old_base, new_base]]
    manifest, config, application_layers = replacement(app, old, new)
    output.mkdir(parents=True, exist_ok=False)
    layout = output/'layout'
    command('crane', 'pull', new_base, str(layout), '--format=oci')
    for layer in application_layers:
        digest = layer['digest']
        if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
            raise ValueError('Unsupported application layer digest')
        target = layout/'blobs'/'sha256'/digest.split(':')[1]
        with target.open('wb') as stream:
            subprocess.run(['crane', 'blob', original.split('@')[0]+'@'+digest], stdout=stream, stderr=subprocess.PIPE, check=True, timeout=600)
    # Validate compressed content before making this layout's index reference it.
    for layer in manifest['layers']:
        if not re.fullmatch(r'sha256:[0-9a-f]{64}', layer['digest']):
            raise ValueError('Unsupported base or application layer digest')
        path = layout/'blobs'/'sha256'/layer['digest'].split(':')[1]
        with path.open('rb') as stream:
            digest = 'sha256:'+hashlib.file_digest(stream, 'sha256').hexdigest()
        if digest != layer['digest'] or path.stat().st_size != layer['size']:
            raise ValueError('Layer bytes do not match manifest')
    manifest['config'] = dict(manifest['config'], **blob(layout, config))
    descriptor = dict(blob(layout, manifest), mediaType=manifest['mediaType'],
                      platform={key: config[key] for key in ['os', 'architecture', 'os.version']})
    (layout/'index.json').write_text(json.dumps({'schemaVersion': 2, 'manifests': [descriptor]}, indent=2)+'\n')
    receipt = {'original': original, 'oldBase': old_base, 'newBase': new_base,
               'imageDigest': descriptor['digest'], 'platform': descriptor['platform'],
               'applicationLayers': application_layers, 'applicationRuntimeConfigPreserved': config.get('config') == app[1].get('config'),
               'allLayerBytesVerified': True, 'scope': 'OCI construction only; native startup and GPU execution remain unqualified'}
    (output/'build.json').write_text(json.dumps(receipt, indent=2)+'\n')
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ['original', 'old-base', 'new-base']:
        parser.add_argument('--'+flag, required=True)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    args = parser.parse_args()
    print(json.dumps(build(args.original, args.old_base, args.new_base, args.output)))


if __name__ == '__main__':
    main()
