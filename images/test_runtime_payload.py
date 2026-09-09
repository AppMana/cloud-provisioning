import hashlib
import io
import pathlib
import tempfile
import unittest
import zipfile

from runtime_payload import PROFILES, stage_runtime


class RuntimeProfiles(unittest.TestCase):
    def worker(self, root, machine_os):
        payload = io.BytesIO()
        with zipfile.ZipFile(payload, 'w') as archive:
            for name in PROFILES[machine_os][0]:
                archive.writestr(name, b'component fixture: '+name.encode())
        worker = root/'k0s'
        worker.write_bytes(b'ELF-or-PE-prefix-fixture'+payload.getvalue())
        return worker, hashlib.sha256(worker.read_bytes()).hexdigest()

    def test_profiles_preserve_exact_names_bytes_and_permissions(self):
        for machine_os in PROFILES:
            with self.subTest(machine_os=machine_os), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                worker, digest = self.worker(root, machine_os)
                result = stage_runtime(worker, digest, root/'out', machine_os=machine_os)
                with zipfile.ZipFile(root/'out/runtime.zip') as archive:
                    self.assertEqual(tuple(archive.namelist()), PROFILES[machine_os][0])
                    for component in result['components']:
                        name = component['name']
                        self.assertEqual(archive.read(name), b'component fixture: '+name.encode())
                        self.assertEqual(archive.getinfo(name).external_attr >> 16, PROFILES[machine_os][1])

    def test_opposite_os_payload_cannot_create_runtime(self):
        for machine_os in PROFILES:
            other = 'linux' if machine_os == 'windows' else 'windows'
            with self.subTest(machine_os=machine_os), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                worker, digest = self.worker(root, machine_os)
                with self.assertRaisesRegex(ValueError, 'exactly one'):
                    stage_runtime(worker, digest, root/'out', machine_os=other)
                self.assertFalse((root/'out').exists())

    def test_unknown_os_does_not_guess_a_profile(self):
        with self.assertRaisesRegex(ValueError, 'unsupported'):
            stage_runtime(pathlib.Path('/not-read'), '0'*64, pathlib.Path('/not-created'), machine_os='unknown')


if __name__ == '__main__': unittest.main()
