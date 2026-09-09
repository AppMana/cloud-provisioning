import copy
import hashlib
import json
import pathlib
import tempfile
import unittest
from unittest import mock

from prepare_cache import build, inputs


class CacheBuildInputs(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.directory = pathlib.Path(temp.name)
        self.digest = 'sha256:1dca17073186db812f380970077f5c82842c810f1bd75800cd926ab50cd34243'
        self.source = 'quay.io/k0sproject/pause@'+self.digest
        self.recipe = dict(schemaVersion=1, machineOS='linux', architecture='amd64',
            workerSHA256='8b5d985f803df27acb44f900b2574a5b48e600bd2f87a3335d2a853b888e9298',
            images=[dict(reference='quay.io/k0sproject/pause:3.10.2-0', pullReference=self.source,
                         digest=self.digest, aliases=[self.source, 'quay.io/k0sproject/pause:3.10.2-0'])])

    def test_native_pause_recipe_has_no_fixed_image_count(self):
        self.assertEqual(inputs(self.recipe, self.directory),
            {name: self.digest for name in self.recipe['images'][0]['aliases']})

    def test_archive_is_verified_before_runtime_starts(self):
        body = b'archive fixture, not a real OCI image'
        archive = self.directory/'pause.tar'
        archive.write_bytes(body)
        item = self.recipe['images'][0]
        del item['pullReference']
        item.update(archive=archive.name, archiveSHA256=hashlib.sha256(body).hexdigest())
        self.assertEqual(len(inputs(self.recipe, self.directory)), 2)
        archive.write_bytes(body+b'changed')
        with self.assertRaisesRegex(ValueError, 'hash mismatch'):
            inputs(self.recipe, self.directory)

    def test_rejects_mutable_pull_and_alias_digest_mismatch(self):
        for change in [dict(pullReference='quay.io/k0sproject/pause:3.10.2-0'),
                       dict(digest='sha256:'+'a'*64), dict(aliases=[self.source, self.source]),
                       dict(aliases=['https://quay.io/pause']), dict(aliases=['other:tag'])]:
            with self.subTest(change=change):
                recipe = copy.deepcopy(self.recipe)
                recipe['images'][0].update(change)
                with self.assertRaises(ValueError):
                    inputs(recipe, self.directory)

    def test_archive_path_cannot_escape_staging_directory(self):
        for name in ['../outside.tar', '/tmp/outside.tar', '.', '..']:
            recipe = copy.deepcopy(self.recipe)
            item = recipe['images'][0]
            del item['pullReference']
            item.update(archive=name, archiveSHA256='a'*64)
            with self.assertRaises(ValueError):
                inputs(recipe, self.directory)

    def test_refuses_symlinked_archive_and_ambiguous_source(self):
        target = self.directory/'target'
        target.write_bytes(b'fixture')
        (self.directory/'pause.tar').symlink_to(target)
        item = self.recipe['images'][0]
        item.update(archive='pause.tar', archiveSHA256=hashlib.sha256(b'fixture').hexdigest())
        with self.assertRaisesRegex(ValueError, 'Choose archive'):
            inputs(self.recipe, self.directory)
        del item['pullReference']
        with self.assertRaisesRegex(ValueError, 'regular archive'):
            inputs(self.recipe, self.directory)

    def test_invalid_profile_and_missing_worker_identity(self):
        for change in [dict(schemaVersion=True), dict(machineOS='windows'), dict(architecture='arm64'),
                       dict(workerSHA256=''), dict(images=[]), dict(images=[None])]:
            recipe = dict(self.recipe, **change)
            with self.assertRaises(ValueError):
                inputs(recipe, self.directory)

    def test_existing_cache_is_not_replayed_or_overwritten(self):
        recipe = self.directory/'recipe.json'
        recipe.write_text(json.dumps(self.recipe))
        cache = self.directory/'cache'
        cache.mkdir()
        original = cache/'original'
        original.write_text('preserve')
        operation = self.directory/'operation'
        with mock.patch('prepare_cache.runtime'), self.assertRaisesRegex(ValueError, 'do not replay'):
            build(recipe, operation, cache)
        self.assertFalse(operation.exists())
        self.assertEqual(original.read_text(), 'preserve')

    def test_existing_operation_and_overlapping_paths_are_rejected(self):
        recipe = self.directory/'recipe.json'
        recipe.write_text(json.dumps(self.recipe))
        operation = self.directory/'operation'
        operation.mkdir()
        with mock.patch('prepare_cache.runtime'), self.assertRaisesRegex(ValueError, 'do not replay'):
            build(recipe, operation, self.directory/'new-cache')
        operation.rmdir()
        with mock.patch('prepare_cache.runtime'), self.assertRaisesRegex(ValueError, 'separate directories'):
            build(recipe, operation, operation/'nested-cache')
        self.assertFalse(operation.exists())


if __name__ == '__main__':
    unittest.main()
