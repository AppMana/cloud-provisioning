"""Select a release-specific published workload for a prepared Windows cache."""
import copy
import pathlib
import re
import sys

# Harness entry points are also invoked by absolute path outside the repo root.
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))
from images.windows.cache import validate_recipe
from harness.e2e.observe.windows_plugin_placement import PROFILES, LABEL


def cache_references(image):
    # CRI normalizes this legacy Docker Hub host before its local lookup,
    # while ctr preserves the original reference in the image store.
    references = {image}
    if image.startswith('index.docker.io/'):
        references.add('docker.io/' + image.removeprefix('index.docker.io/'))
    return references


def require_plugin_cache(daemonsets, recipe, year):
    """Require the actual build-selected test profile before VM provisioning."""
    validate_recipe(recipe, year)
    build = '10.0.' + str(recipe['windowsBuild'])
    matches = [d for d in daemonsets.get('items', [])
               if d.get('metadata', {}).get('name') == PROFILES[build]]
    if len(matches) != 1:
        raise ValueError('Require exactly one selected Windows GPU plugin profile')
    spec = matches[0].get('spec', {}).get('template', {}).get('spec', {})
    if spec.get('nodeSelector') != {
            'kubernetes.io/os': 'windows', 'kubernetes.io/arch': 'amd64',
            'node.kubernetes.io/windows-build': build, LABEL: 'true'} or spec.get('affinity'):
        raise ValueError('Plugin profile must select the expected Windows build')
    containers = spec.get('initContainers', []) + spec.get('containers', [])
    images = [c.get('image', '') for c in containers]
    if not images or any(not re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}', image)
                         for image in images):
        raise ValueError('Plugin profile requires pinned container images')
    required = set().union(*(cache_references(image) for image in images))
    missing = sorted(required - set(recipe['images']))
    if missing:
        raise ValueError('Selected GPU plugin images missing from cache recipe: ' + ', '.join(missing))
    return {'profile': PROFILES[build], 'images': sorted(set(images)),
            'cacheReferences': sorted(required)}


def select_workload(inventory, recipe, year):
    validate_recipe(recipe, year)
    record = inventory.get('windowsGPUProbeByWindowsVersion', {}).get(year, {})
    platform = record.get('platform', {})
    build = {'2022': '20348', '2025': '26100'}[year]
    version = platform.get('os.version', '').split('.')
    image = record.get('image', '')
    if (platform.get('os') != 'windows' or platform.get('architecture') != 'amd64'
            or len(version) != 4 or version[:3] != ['10', '0', build]
            or not version[3].isdigit()
            or not re.fullmatch(r'[^\s@]+@sha256:[a-f0-9]{64}', image)
            or not cache_references(image) <= set(recipe['images'])):
        raise ValueError('Require the selected Windows publication in its matching cache recipe')
    secret = record.get('pullSecret', '')
    if not isinstance(secret, str) or not re.fullmatch(r'[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?', secret):
        raise ValueError('Versioned workload publication must record its pull Secret')
    return copy.deepcopy(record)
