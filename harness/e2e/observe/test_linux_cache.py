import copy
import json
import pathlib
import unittest
from linux_cache import evaluate, expected_images, inventory


class NativeLinuxCache(unittest.TestCase):
    def setUp(self):
        self.native = json.loads((pathlib.Path(__file__).parent/'testdata/linux-pause-cache-native.json').read_text())

    def test_actual_pause_cache_survived_restart_without_qualifying_clone(self):
        result = evaluate(**self.native)
        self.assertTrue(result['passed'])
        self.assertEqual(result['references'], 2)
        self.assertFalse(result['qualificationEligible'])

    def test_actual_gpu_cache_has_all_23_references_across_restart(self):
        for fixture in ['linux-gpu-cache-native.json', 'linux-public-full-cache-native.json']:
            with self.subTest(fixture=fixture):
                native = json.loads((pathlib.Path(__file__).parent/'testdata'/fixture).read_text())
                result = evaluate(**native)
                self.assertTrue(result['passed'])
                self.assertEqual(result['references'], 23)
                self.assertFalse(result['qualificationEligible'])
                row = native['observation']['after'].splitlines()[1].split()
                row[0] = 'unexpected:latest'
                native['observation']['after'] += ' '.join(row)+'\n'
                self.assertFalse(evaluate(**native)['passed'])

    def test_malformed_recipe_types_are_rejected(self):
        for recipe in [None, dict(self.native['recipe'], schemaVersion=True),
                       dict(self.native['recipe'], images=[None]),
                       dict(self.native['recipe'], images=[{'digest': None}])]:
            with self.subTest(recipe=recipe), self.assertRaises(ValueError):
                expected_images(recipe)

    def test_complete_content_without_unpack_or_with_missing_content_fails(self):
        for before, after in [(' true', ' false'), ('complete (2/2)', 'incomplete (1/2)'), ('complete (2/2)', 'complete (1/2)')]:
            with self.subTest(after=after):
                native = copy.deepcopy(self.native)
                native['observation']['after'] = native['observation']['after'].replace(before, after)
                self.assertFalse(evaluate(**native)['passed'])

    def test_changed_digest_or_missing_alias_cannot_pass(self):
        native = copy.deepcopy(self.native)
        digest = native['recipe']['images'][0]['digest']
        native['observation']['after'] = native['observation']['after'].replace(digest, 'sha256:'+'0'*64)
        self.assertFalse(evaluate(**native)['passed'])
        native = copy.deepcopy(self.native)
        native['observation']['readyAfter'] = native['observation']['readyAfter'].splitlines()[0]+'\n'
        self.assertFalse(evaluate(**native)['passed'])

    def test_runtime_shutdown_is_required_even_when_images_are_ready(self):
        self.native['observation']['stops'][1]['originalProcessExited'] = False
        self.assertFalse(evaluate(**self.native)['passed'])

    def test_duplicate_table_rows_are_rejected(self):
        text = self.native['observation']['before']
        with self.assertRaisesRegex(ValueError, 'Duplicate'):
            inventory(text+text.splitlines()[1]+'\n')

    def test_wrong_os_or_ambiguous_recipe_is_rejected(self):
        for change in ['os', 'duplicate', 'digest']:
            recipe = copy.deepcopy(self.native['recipe'])
            if change == 'os': recipe['machineOS'] = 'windows'
            elif change == 'duplicate': recipe['images'].append(copy.deepcopy(recipe['images'][0]))
            else: recipe['images'][0]['digest'] = 'sha256:'+'0'*64
            with self.assertRaises(ValueError): expected_images(recipe)


if __name__ == '__main__': unittest.main()
