import copy
import json
import pathlib
import unittest

from linux_cache_reuse import evaluate


class NativeCacheReuse(unittest.TestCase):
    def setUp(self):
        self.native = json.loads((pathlib.Path(__file__).parent/
                                  'testdata/linux-cache-reuse-native.json').read_text())

    def test_actual_fresh_gpu_worker_reused_content_and_unpacked_layers(self):
        result = evaluate(**self.native)
        self.assertTrue(result['passed'])
        self.assertEqual((result['references'], result['blobs'], result['snapshotChains']), (23, 161, 12))
        self.assertFalse(result['qualificationEligible'])

    def test_missing_layer_cannot_hide_behind_ready_image_inventory(self):
        blobs = self.native['observation']['blobs']
        del blobs[next(iter(blobs))]
        self.assertFalse(evaluate(**self.native)['passed'])

    def test_content_written_after_launch_fails_reuse(self):
        blob = next(iter(self.native['observation']['blobs'].values()))
        blob['ctimeNs'] = 9_000_000_000_000_000_000
        self.assertFalse(evaluate(**self.native)['passed'])

    def test_recreated_or_wrong_snapshot_does_not_prove_baked_unpack(self):
        for key, value in [('Created', '2099-01-01T00:00:00Z'), ('Kind', 'Active'), ('Name', 'wrong-chain')]:
            native = copy.deepcopy(self.native)
            next(iter(native['observation']['snapshots'].values()))['info'][key] = value
            with self.subTest(key=key):
                self.assertFalse(evaluate(**native)['passed'])

    def test_preserved_mtime_alone_does_not_prove_runtime_reuse(self):
        self.native['observation']['fileIdentities']['/var/lib/k0s/bin/containerd']['ctimeNs'] += 1
        self.assertFalse(evaluate(**self.native)['passed'])

    def test_parent_image_cannot_pass_child_cache_acceptance(self):
        self.native['binding']['imageID'] = 'ami-007650e971dd51a9d'
        self.assertFalse(evaluate(**self.native)['passed'])


if __name__ == '__main__':
    unittest.main()
