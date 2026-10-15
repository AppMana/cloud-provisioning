import hashlib
import json
import unittest

from linux_cache_reuse_native import descriptor_contract


class NativeDescriptorClosure(unittest.TestCase):
    def setUp(self):
        self.blobs = {}
        self.layers = ['sha256:'+'1'*64, 'sha256:'+'2'*64]
        self.diffs = ['sha256:'+'3'*64, 'sha256:'+'4'*64]
        self.config = self.add(dict(os='linux', architecture='arm64', rootfs=dict(type='layers', diff_ids=self.diffs)))
        self.manifest = self.add(dict(config=dict(digest=self.config), layers=[dict(digest=d) for d in self.layers]))
        self.index = self.add(dict(manifests=[
            dict(digest=self.manifest, platform=dict(os='linux', architecture='arm64')),
            dict(digest='sha256:'+'5'*64, platform=dict(os='linux', architecture='amd64'))]))

    def add(self, value):
        data = json.dumps(value).encode()
        digest = 'sha256:'+hashlib.sha256(data).hexdigest()
        self.blobs[digest] = data
        return digest

    def test_native_closure_excludes_other_architectures(self):
        result = descriptor_contract(self.index, 'arm64', self.blobs.__getitem__)
        self.assertEqual(set(result['requiredBlobs']), {self.index, self.manifest, self.config, *self.layers})
        self.assertEqual(result['chainID'], 'sha256:'+hashlib.sha256(' '.join(self.diffs).encode()).hexdigest())

    def test_config_cannot_misrepresent_index_platform(self):
        config = self.add(dict(os='linux', architecture='amd64', rootfs=dict(type='layers', diff_ids=self.diffs)))
        manifest = self.add(dict(config=dict(digest=config), layers=[dict(digest=d) for d in self.layers]))
        with self.assertRaisesRegex(ValueError, 'config differs'):
            descriptor_contract(manifest, 'arm64', self.blobs.__getitem__)

    def test_mutated_descriptor_bytes_are_rejected(self):
        self.blobs[self.config] += b' '
        with self.assertRaisesRegex(ValueError, 'content hash'):
            descriptor_contract(self.index, 'arm64', self.blobs.__getitem__)

    def test_duplicate_native_candidates_are_ambiguous(self):
        value = json.loads(self.blobs[self.index])
        value['manifests'].append(value['manifests'][0])
        index = self.add(value)
        with self.assertRaisesRegex(ValueError, 'exactly one'):
            descriptor_contract(index, 'arm64', self.blobs.__getitem__)

    def test_layer_count_mismatch_is_rejected(self):
        manifest = self.add(dict(config=dict(digest=self.config), layers=[dict(digest=self.layers[0])]))
        with self.assertRaisesRegex(ValueError, 'layer and diff-ID'):
            descriptor_contract(manifest, 'arm64', self.blobs.__getitem__)

    def test_invalid_digest_cannot_be_used_as_a_file_path(self):
        with self.assertRaisesRegex(ValueError, 'SHA256 descriptor'):
            descriptor_contract('../../outside', 'arm64', lambda _: self.fail('read before validation'))


if __name__ == '__main__':
    unittest.main()
