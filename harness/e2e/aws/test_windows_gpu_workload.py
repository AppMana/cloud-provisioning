import copy
import json
import pathlib
import os
import subprocess
import sys
import tempfile
import unittest

from windows_gpu_workload import select_workload, require_plugin_cache
from images.windows.cache import make_recipe


class WorkloadSelection(unittest.TestCase):
    def test_import_from_external_working_directory_without_pythonpath(self):
        helper_dir = pathlib.Path(__file__).resolve().parent
        env = dict(os.environ)
        env.pop('PYTHONPATH', None)
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([sys.executable, '-c',
                'import sys; sys.path.insert(0, sys.argv[1]); '
                'from windows_gpu_workload import select_workload', str(helper_dir)],
                cwd=directory, env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)

    def setUp(self):
        root = pathlib.Path(__file__).resolve().parents[3]
        publication = json.loads((root / 'docs/validation/windows-workload-platform-publication-results.json').read_text())
        cache = json.loads((root / 'harness/e2e/observe/testdata/windows-cache-complete-ready-2022.json').read_text())
        self.recipe = cache['recipe']
        self.record = dict(image=publication['published2022Image'],
                           platform=publication['build']['platform'],
                           pullSecret='cldt-windows-v2-gpu-probe-pull')
        self.inventory = dict(windowsGPUProbeImage=publication['preservedLegacy2025Image'],
                              windowsGPUProbeByWindowsVersion={'2022': self.record})

    def test_native_2022_recipe_selects_2022_image_despite_legacy_2025(self):
        selected = select_workload(self.inventory, self.recipe, '2022')
        self.assertEqual(selected, self.record)
        selected['platform']['os.version'] = 'changed'
        self.assertEqual(self.record['platform']['os.version'], '10.0.20348.5499')

    def test_no_fallback_to_legacy_or_uncached_publication(self):
        for change in [dict(platform={'os': 'windows', 'architecture': 'amd64',
                                      'os.version': '10.0.26100.33296'}),
                       dict(image=self.inventory['windowsGPUProbeImage']),
                       dict(pullSecret=''), dict(platform={})]:
            inventory = copy.deepcopy(self.inventory)
            inventory['windowsGPUProbeByWindowsVersion']['2022'].update(change)
            with self.subTest(change=change), self.assertRaises(ValueError):
                select_workload(inventory, self.recipe, '2022')
        with self.assertRaises(ValueError):
            select_workload({'windowsGPUProbeImage': self.record['image']}, self.recipe, '2022')
        with self.assertRaises(ValueError):
            select_workload(self.inventory, self.recipe, '2025')

    def test_observed_2022_profile_rejects_recipe_with_only_2025_plugin(self):
        root = pathlib.Path(__file__).resolve().parents[1] / 'observe/testdata'
        profiles = json.loads((root / 'windows-gpu-plugin-cache-profiles.json').read_text())
        image = 'index.docker.io/tensorworks/wddm-device-plugin@sha256:47f97c76c16ad24490caf1d344b4a9bc0ccc05fe787e2b68fe5a1051818ce9ec'
        with self.assertRaisesRegex(ValueError, 'missing from cache recipe'):
            require_plugin_cache(profiles, self.recipe, '2022')
        # A proposed next recipe passes admission only, not native cache reuse.
        observed = json.loads((root / 'windows-cri-reference-alias-native.json').read_text())
        index_only = observed['recipe']
        self.assertIn(observed['originalReference'], observed['registeredImages'])
        self.assertNotIn(observed['canonicalReference'], observed['registeredImages'])
        with self.assertRaisesRegex(ValueError, 'missing from cache recipe: docker.io/'):
            require_plugin_cache(profiles, index_only, '2022')
        canonical = image.replace('index.docker.io/', 'docker.io/', 1)
        proposed = make_recipe(self.recipe['runtime'], self.recipe['images'] + [image, canonical],
                               '2022', self.recipe['aliases'])
        self.assertEqual(require_plugin_cache(profiles, proposed, '2022'),
                         {'profile': 'windows-wddm-device-plugin', 'images': [image],
                          'cacheReferences': sorted([image, canonical])})
        for change in ['absent', 'duplicate', 'selector', 'affinity', 'unpinned']:
            altered = copy.deepcopy(profiles)
            selected = next(d for d in altered['items'] if d['metadata']['name'] == 'windows-wddm-device-plugin')
            spec = selected['spec']['template']['spec']
            if change == 'absent': altered['items'].remove(selected)
            elif change == 'duplicate': altered['items'].append(copy.deepcopy(selected))
            elif change == 'selector': spec['nodeSelector']['node.kubernetes.io/windows-build'] = '10.0.26100'
            elif change == 'affinity': spec['affinity'] = {'nodeAffinity': {}}
            else: spec['containers'][0]['image'] = 'example/plugin:latest'
            with self.subTest(change=change), self.assertRaises(ValueError):
                require_plugin_cache(altered, proposed, '2022')
