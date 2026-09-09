"""Shared Windows image-cache recipe and native readiness checks."""
import hashlib
import json
import re
import argparse
import pathlib


def validate_aliases(images, aliases):
    aliases = list(aliases or [])
    names = set(images)
    for alias in aliases:
        if not isinstance(alias, dict) or set(alias) != {'reference', 'image'}:
            raise ValueError('Alias requires exactly reference and image')
        reference, image = alias['reference'], alias['image']
        if (not isinstance(reference, str) or not isinstance(image, str) or
                image not in images or reference in names or
                not re.fullmatch(r'[^\s@=]+:[A-Za-z0-9_][A-Za-z0-9_.-]*', reference)):
            raise ValueError('Require a unique explicit tag alias of a pinned recipe image')
        def repository(value):
            value = value.split('@')[0]
            prefix, _, leaf = value.rpartition('/')
            return (prefix + '/' if prefix else '') + leaf.split(':')[0]
        if repository(reference) != repository(image):
            raise ValueError('Alias and pinned image must name the same repository')
        names.add(reference)
    return sorted(aliases, key=lambda item: item['reference'])


def make_recipe(runtime, images, year, aliases=None):
    images, _ = validate_inputs(runtime, images)
    aliases = validate_aliases(images, aliases)
    recipe = {'schemaVersion': 2, 'windowsBuild': {'2022': 20348, '2025': 26100}[year],
              'dataDirectoryPermissions': 'inherit-parent',
              'images': sorted(images), 'runtime': {
                  'schemaVersion': 1, 'workerSHA256': runtime['workerSHA256'],
                  'components': sorted([{'name': c['name'], 'sha256': c['sha256']}
                                        for c in runtime['components']], key=lambda c: c['name'])}}
    if aliases:
        recipe.update(schemaVersion=3, aliases=aliases)
    recipe['sha256'] = hashlib.sha256(json.dumps(recipe, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    return recipe


def validate_recipe(recipe, year):
    expected = make_recipe(recipe['runtime'], recipe['images'], year, recipe.get('aliases'))
    if recipe != expected:
        raise ValueError('Cache recipe identity, OS or digest mismatch')
    return recipe


def validate_extension(previous, recipe, year):
    validate_recipe(previous, year)
    validate_recipe(recipe, year)
    if previous['runtime'] != recipe['runtime']:
        raise ValueError('Cache extension must preserve the worker and runtime')
    def targets(value):
        result = {image: image.split('@')[1] for image in value['images']}
        result.update({item['reference']: item['image'].split('@')[1] for item in value.get('aliases', [])})
        return result
    old, new = targets(previous), targets(recipe)
    if old == new or any(new.get(reference) != digest for reference, digest in old.items()):
        raise ValueError('Cache extension must add references without removing or retargeting existing ones')
    return recipe


def verify_preparation(receipt, recipe, year):
    validate_recipe(recipe, year)
    result = evaluate(receipt['snapshot'], recipe['runtime'], recipe['images'], year, recipe.get('aliases'))
    result['checks']['dataDirectoryInheritsPermissions'] = receipt['snapshot'].get('dataDirectoryInheritsPermissions') is True
    result['passed'] = all(result['checks'].values())
    if (not result['passed'] or receipt.get('serviceStopped') is not True or
            receipt.get('cacheServiceRemoved') is not True):
        raise ValueError('Cache preparation requires inherited data-directory permissions, unpacked images and a stopped, removed build service')
    return result


def validate_inputs(runtime, images):
    images = list(images)
    if not images or len(set(images)) != len(images) or any(
            not re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}', image) for image in images):
        raise ValueError('Require unique digest-pinned image references')
    components = {c['name']: c['sha256'] for c in runtime['components']}
    if (runtime.get('schemaVersion') != 1 or len(runtime['components']) != 2 or
            set(components) != {'containerd.exe', 'containerd-shim-runhcs-v1.exe'} or
            any(not re.fullmatch('[a-f0-9]{64}', value) for value in
                [runtime.get('workerSHA256', '')] + list(components.values()))):
        raise ValueError('Require the exact staged worker and runtime identity')
    return images, components


def evaluate(snapshot, runtime, images, year, aliases=None):
    images, components = validate_inputs(runtime, images)
    aliases = validate_aliases(images, aliases)
    references = images + [item['reference'] for item in aliases]
    checks = {
        'expectedWindowsBuild': snapshot.get('windowsBuild') == {'2022': 20348, '2025': 26100}[year],
        'workerMatches': snapshot.get('workerSHA256') == runtime['workerSHA256'],
        'runtimeMatches': snapshot.get('containerdSHA256') == components['containerd.exe'] and
                          snapshot.get('shimSHA256') == components['containerd-shim-runhcs-v1.exe'],
        'k0sRuntimeStore': snapshot.get('root') == r'C:\var\lib\k0s\containerd' and
                           snapshot.get('namespace') == 'k8s.io' and snapshot.get('snapshotter') == 'windows',
        'cacheServiceRunning': snapshot.get('cacheService') == 'Running',
        'noWorkerServiceOrKnownIdentity': snapshot.get('workerServicePresent') is False and
                                         snapshot.get('identityPathsPresent') == [],
        'noWorkloadContainersOrTasks': snapshot.get('containers') == [] and snapshot.get('tasks') == [],
        'exactImagesRegistered': sorted(snapshot.get('registeredImages', [])) == sorted(references),
        'allImagesDownloadedAndUnpacked': sorted(snapshot.get('readyImages', [])) == sorted(references),
    }
    if aliases:
        targets = {image: image.split('@')[1] for image in images}
        targets.update({item['reference']: item['image'].split('@')[1] for item in aliases})
        checks['exactImageTargets'] = snapshot.get('imageTargets') == targets
    return {'passed': all(checks.values()), 'checks': checks,
            'observedAt': snapshot.get('observedAt'), 'images': images,
            'scope': 'Running builder cache readiness only; service shutdown, Sysprep and fresh-clone reuse remain separate gates'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', required=True, type=pathlib.Path)
    parser.add_argument('--image', required=True, action='append')
    parser.add_argument('--alias', action='append', default=[], metavar='TAG=PINNED_IMAGE')
    parser.add_argument('--windows-version', required=True, choices=['2022', '2025'])
    parser.add_argument('--output', required=True, type=pathlib.Path)
    args = parser.parse_args()
    aliases = []
    for item in args.alias:
        if item.count('=') != 1:
            parser.error('alias must be TAG=PINNED_IMAGE')
        reference, image = item.split('=')
        aliases.append(dict(reference=reference, image=image))
    recipe = make_recipe(json.loads(args.runtime.read_text()), args.image, args.windows_version, aliases)
    with args.output.open('x') as stream:
        stream.write(json.dumps(recipe, indent=2) + '\n')
    print(recipe['sha256'])


if __name__ == '__main__':
    main()
