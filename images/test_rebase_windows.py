import copy
import json
import pathlib
import unittest
from rebase_windows import replacement, pinned


class RebaseTest(unittest.TestCase):
    def setUp(self):
        self.f = json.loads((pathlib.Path(__file__).parent/'testdata/windows-rebase-native.json').read_text())

    def test_native_manifest_rebase_preserves_application_and_updates_platform(self):
        f = self.f
        before = copy.deepcopy(f)
        manifest, config, layers = replacement(f['original'], f['old'], f['new'])
        self.assertEqual(config['os.version'], '10.0.26100.33296')
        self.assertEqual(layers, f['original'][0]['layers'][2:])
        self.assertEqual(manifest['layers'], f['new'][0]['layers'] + layers)
        self.assertEqual(config['config'], f['original'][1]['config'])
        self.assertEqual(config['config']['Entrypoint'], ['C:\\ffmpeg.exe'])
        self.assertEqual(config['rootfs']['diff_ids'], f['new'][1]['rootfs']['diff_ids'] + f['original'][1]['rootfs']['diff_ids'][2:])
        self.assertEqual(f, before)

    def test_incorrect_base_cannot_remove_unrelated_layers(self):
        changes = {
            'compressed digest': lambda f: f['old'][0]['layers'][0].update(digest='sha256:'+'0'*64),
            'uncompressed digest': lambda f: f['old'][1]['rootfs']['diff_ids'].__setitem__(0,'sha256:'+'0'*64),
            'history': lambda f: f['old'][1]['history'][0].update(created_by='unrelated'),
            'old OS': lambda f: f['old'][1].update({'os.version':'10.0.20348.1'}),
            'Linux target': lambda f: f['new'][1].update(os='linux'),
            'ARM target': lambda f: f['new'][1].update(architecture='arm64'),
            'wrong layer count': lambda f: f['new'][1]['rootfs']['diff_ids'].append('sha256:'+'0'*64),
        }
        original = copy.deepcopy(self.f)
        for name, change in changes.items():
            with self.subTest(name=name):
                f = copy.deepcopy(original);change(f)
                with self.assertRaises(ValueError):replacement(f['original'],f['old'],f['new'])

    def test_base_only_image_is_not_an_application_rebase(self):
        with self.assertRaises(ValueError):replacement(self.f['old'], self.f['old'], self.f['new'])

    def test_mutable_tags_rejected(self):
        with self.assertRaises(ValueError):pinned('mcr.microsoft.com/windows/server:ltsc2025')


if __name__ == '__main__':unittest.main()
