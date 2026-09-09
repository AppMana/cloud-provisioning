import hashlib
import json
import pathlib
import tempfile
import types
import unittest

from prepare_k0s import prepare


class PrepareK0s(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.artifact = self.root/'artifact'
        self.artifact.write_bytes(b'fixture executable')
        self.sha = hashlib.sha256(self.artifact.read_bytes()).hexdigest()
        self.version = 'v1.36.2+k0s.0'
        self.calls = []

    def execute(self, args, **kwargs):
        self.calls.append(args)
        return types.SimpleNamespace(stdout='not-found\n' if args[0] == 'systemctl' else self.version+'\n')

    def test_prepared_binary_and_receipt_are_reusable_without_service_creation(self):
        first = prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        target = self.root/'usr/local/bin/k0s'
        before = target.stat().st_mtime_ns
        second = prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        self.assertEqual(first, second)
        self.assertEqual(before, target.stat().st_mtime_ns)
        self.assertEqual(target.read_bytes(), self.artifact.read_bytes())
        self.assertEqual(target.stat().st_mode & 0o777, 0o755)
        self.assertTrue(all(args[0] == 'systemctl' or args[1:] == ['version'] for args in self.calls))

    def test_checksum_and_native_version_must_match_before_installation(self):
        with self.assertRaisesRegex(ValueError, 'checksum'):
            prepare(self.artifact, '0'*64, self.version, self.root, self.execute)
        self.version = 'v1.36.1+k0s.0'
        with self.assertRaisesRegex(ValueError, 'Native executable version'):
            prepare(self.artifact, self.sha, 'v1.36.2+k0s.0', self.root, self.execute)
        self.assertFalse((self.root/'usr/local/bin/k0s').exists())
        self.assertEqual(list((self.root/'usr/local/bin').iterdir()), [])

    def test_joined_state_and_existing_services_are_rejected(self):
        state = self.root/'var/lib/k0s'
        state.mkdir(parents=True)
        (state/'identity').write_text('existing identity')
        with self.assertRaisesRegex(ValueError, 'runtime state'):
            prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        (state/'identity').unlink()
        with self.assertRaisesRegex(ValueError, 'service'):
            prepare(self.artifact, self.sha, self.version, self.root,
                    lambda *args, **kwargs: types.SimpleNamespace(stdout='loaded\n'))
        self.assertFalse((self.root/'usr/local/bin/k0s').exists())

    def test_different_binary_or_stale_receipt_cannot_be_replaced(self):
        prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        receipt = self.root/'etc/cloud-provisioning-image/k0s.json'
        value = json.loads(receipt.read_text()); value['sha256'] = '0'*64
        receipt.write_text(json.dumps(value))
        with self.assertRaisesRegex(ValueError, 'receipt'):
            prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        target = self.root/'usr/local/bin/k0s'; target.write_bytes(b'other executable')
        with self.assertRaisesRegex(ValueError, 'different installed'):
            prepare(self.artifact, self.sha, self.version, self.root, self.execute)
        self.assertEqual(target.read_bytes(), b'other executable')
