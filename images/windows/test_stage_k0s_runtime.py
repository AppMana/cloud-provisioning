import hashlib
import io
import json
import pathlib
import tempfile
import unittest
import warnings
import zipfile

from stage_k0s_runtime import COMPONENTS, stage


class AppendedRuntimePayload(unittest.TestCase):
    def make_worker(self, root, names=COMPONENTS):
        payload = io.BytesIO()
        with warnings.catch_warnings(), zipfile.ZipFile(payload, 'w', compression=zipfile.ZIP_DEFLATED) as archive:
            warnings.simplefilter('ignore', UserWarning)
            for name in names:
                archive.writestr(name, ('fixture '+name).encode())
        # Match k0s's observed appended archive: ZIP offsets remain relative
        # to the payload, rather than the preceding executable's start.
        path = root/'worker.exe'
        path.write_bytes(b'MZ executable prefix fixture'+payload.getvalue())
        return path, hashlib.sha256(path.read_bytes()).hexdigest()

    def test_appended_payload_becomes_conventional_zip_with_exact_components(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            worker, digest = self.make_worker(root, (*COMPONENTS, '../unselected'))
            result = stage(worker, digest, root/'output')
            archive_path = root/'output/runtime.zip'
            self.assertTrue(archive_path.read_bytes().startswith(b'PK\x03\x04'))
            with zipfile.ZipFile(archive_path) as archive:
                self.assertEqual(tuple(archive.namelist()), COMPONENTS)
                for component in result['components']:
                    self.assertEqual(hashlib.sha256(archive.read(component['name'])).hexdigest(), component['sha256'])
            self.assertEqual(result, json.loads((root/'output/runtime.json').read_text()))
            self.assertEqual(result['runtimeArchiveSHA256'],hashlib.sha256(archive_path.read_bytes()).hexdigest())

    def test_hash_mismatch_does_not_create_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            worker, _ = self.make_worker(root)
            with self.assertRaisesRegex(ValueError, 'SHA-256'):
                stage(worker, '0'*64, root/'output')
            self.assertFalse((root/'output').exists())

    def test_missing_or_ambiguous_runtime_payload_is_rejected(self):
        for names in [COMPONENTS[:1], (*COMPONENTS, COMPONENTS[0])]:
            with self.subTest(names=names), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                worker, digest = self.make_worker(root, names)
                with self.assertRaisesRegex(ValueError, 'exactly one'):
                    stage(worker, digest, root/'output')
                self.assertFalse((root/'output').exists())

    def test_existing_build_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            worker, digest = self.make_worker(root)
            output = root/'output'
            stage(worker, digest, output)
            original = (output/'runtime.zip').read_bytes()
            with self.assertRaises(FileExistsError):
                stage(worker, digest, output)
            self.assertEqual(original, (output/'runtime.zip').read_bytes())


if __name__ == '__main__':
    unittest.main()
