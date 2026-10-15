"""Read fresh Linux worker cache evidence without pulling or importing images.

The provider must supply the observed instance/image/launch binding. Run before
harness image imports; this collector cannot establish that ordering by itself.
"""
import argparse
import hashlib
import json
import os
import pathlib
import platform
import subprocess

from linux_cache import DIGEST, expected_images, inventory
from linux_cache_native import file_identities, require
from linux_cache_reuse import evaluate

BLOBS = pathlib.Path('/var/lib/k0s/containerd/io.containerd.content.v1.content/blobs/sha256')
WORKER = pathlib.Path('/usr/local/bin/k0s')
BIN = pathlib.Path('/var/lib/k0s/bin')


def descriptor_contract(target, architecture, read):
    required = set()

    def document(digest):
        require(isinstance(digest, str) and DIGEST.fullmatch(digest), 'Require SHA256 descriptor')
        raw = read(digest)
        require(len(raw) <= 16 << 20 and 'sha256:'+hashlib.sha256(raw).hexdigest() == digest,
                'Descriptor content hash or size differs')
        required.add(digest)
        return json.loads(raw)

    current = target
    for _ in range(5):
        manifest = document(current)
        if 'manifests' not in manifest:
            break
        selected = [item for item in manifest['manifests']
                    if item.get('platform', {}).get('os') == 'linux'
                    and item['platform'].get('architecture') == architecture
                    and item['platform'].get('variant', '') in ('', 'v8')]
        require(len(selected) == 1, 'Require exactly one native Linux manifest')
        current = selected[0]['digest']
    else:
        raise ValueError('Image index nesting is excessive')
    config = document(manifest['config']['digest'])
    require(config.get('os') == 'linux' and config.get('architecture') == architecture,
            'Image config differs from the native Linux architecture')
    diffs = config['rootfs']['diff_ids']
    require(config['rootfs']['type'] == 'layers' and diffs and len(diffs) == len(manifest['layers']),
            'Require matching nonempty layer and diff-ID lists')
    for digest in diffs:
        require(isinstance(digest, str) and DIGEST.fullmatch(digest), 'Invalid layer diff ID')
    for layer in manifest['layers']:
        digest = layer['digest']
        require(isinstance(digest, str) and DIGEST.fullmatch(digest), 'Invalid layer digest')
        required.add(digest)
    chain = diffs[0]
    for digest in diffs[1:]:
        chain = 'sha256:'+hashlib.sha256((chain+' '+digest).encode()).hexdigest()
    return dict(requiredBlobs=sorted(required), chainID=chain, nativeArchitecture=architecture)


def observe(recipe, binding, candidate, operation):
    expected = expected_images(recipe)
    architecture = {'x86_64': 'amd64', 'aarch64': 'arm64'}.get(platform.machine())
    require(platform.system() == 'Linux' and architecture == recipe['architecture'], 'Native architecture mismatch')
    require(os.geteuid() == 0, 'Run as root on the fresh worker')
    require(operation.is_absolute() and operation == operation.resolve() and not operation.exists(),
            'Require a new absolute observation directory without symlinks')
    operation.mkdir(mode=0o700)

    def save(name, value):
        with (operation/name).open('x') as out:
            json.dump(value, out, indent=2)

    save('intent.json', dict(recipe=recipe, binding=binding, candidateImage=candidate))
    capture = json.loads(pathlib.Path('/etc/cloud-provisioning-image/linux-cache-capture.json').read_text())
    require(capture['nativeArchitecture'] == architecture and capture['recipeSHA256'] ==
            hashlib.sha256(json.dumps(recipe, sort_keys=True).encode()).hexdigest(), 'Capture recipe or architecture differs')
    ctr = [str(WORKER), 'ctr', '--namespace', 'k8s.io']

    def call(name, args):
        result = subprocess.run(ctr+args, capture_output=True, text=True, timeout=60)
        save(name+'.json', dict(argv=args, exitCode=result.returncode, stdout=result.stdout, stderr=result.stderr))
        require(result.returncode == 0, 'Native cache observation failed: '+name)
        return result.stdout

    def read(digest):
        path = BLOBS/digest.split(':', 1)[1]
        require(not path.is_symlink() and path.is_file() and path.stat().st_size <= 16 << 20,
                'Require a bounded regular descriptor')
        return path.read_bytes()

    contracts = {digest: descriptor_contract(digest, architecture, read) for digest in sorted(set(expected.values()))}
    save('contracts.json', contracts)
    blobs = {}
    for digest in sorted({blob for contract in contracts.values() for blob in contract['requiredBlobs']}):
        path = BLOBS/digest.split(':', 1)[1]
        identity = file_identities([path])[str(path)]
        require('sha256:'+identity['sha256'] == digest, 'Cached content hash differs')
        blobs[digest] = identity
    snapshots = {}
    for number, (digest, contract) in enumerate(contracts.items()):
        info = json.loads(call('snapshot-'+str(number), ['snapshots', '--snapshotter', 'overlayfs', 'info', contract['chainID']]))
        snapshots[digest] = dict(chainID=contract['chainID'], info=info)
    images_check = call('images-check', ['images', 'check'])
    rows = inventory(images_check)
    observation = dict(nativeArchitecture=architecture, imagesCheck=images_check,
        ready=call('images-ready', ['images', 'check', '--quiet']),
        images={name: rows[name]['digest'] for name in expected if name in rows},
        blobs=blobs, snapshots=snapshots, captureReceipt=capture,
        fileIdentities=file_identities([WORKER, *(BIN/name for name in ('containerd', 'containerd-shim-runc-v2', 'runc'))]))
    save('observation.json', observation)
    result = evaluate(candidate, recipe, binding, contracts, observation)
    save('result.json', result)
    require(result['passed'], 'Fresh-worker reuse checks failed; original evidence retained')
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--recipe', type=pathlib.Path, required=True)
    parser.add_argument('--binding', type=pathlib.Path, required=True)
    parser.add_argument('--candidate-image', required=True)
    parser.add_argument('--operation', type=pathlib.Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    print(json.dumps(observe(json.loads(args.recipe.read_text()), json.loads(args.binding.read_text()),
                             args.candidate_image, args.operation)))


if __name__ == '__main__':
    main()
