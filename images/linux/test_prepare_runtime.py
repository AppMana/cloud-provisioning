import hashlib
import io
import json
import pathlib
import stat
import subprocess
import tempfile
import unittest
import zipfile

from prepare_runtime import COMPONENTS, prepare


class RuntimePreparation(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = pathlib.Path(temporary.name)
        self.metadata = self.root/'etc/cloud-provisioning-image'
        self.metadata.mkdir(parents=True)
        self.worker = self.root/'usr/local/bin/k0s'
        self.worker.parent.mkdir(parents=True)
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, 'w') as archive:
            for name in COMPONENTS:
                archive.writestr(name, ('embedded '+name).encode())
        self.worker.write_bytes(b'ELF prefix fixture'+buffer.getvalue())
        self.runtime = self.root/'runtime.zip'
        self.runtime.write_bytes(buffer.getvalue())
        worker_hash = hashlib.sha256(self.worker.read_bytes()).hexdigest()
        (self.metadata/'k0s.json').write_text(json.dumps(dict(sha256=worker_hash)))
        self.manifest = self.root/'runtime.json'
        self.values = dict(schemaVersion=1, workerSHA256=worker_hash,
                           runtimeArchiveSHA256=hashlib.sha256(self.runtime.read_bytes()).hexdigest(),
                           components=[dict(name=name, bytes=len('embedded '+name),
                                            sha256=hashlib.sha256(('embedded '+name).encode()).hexdigest()) for name in COMPONENTS])
        self.save_manifest()
        self.calls = []
        self.native_version = 'containerd github.com/containerd/containerd/v2 2.3.2 fixture'

    def save_manifest(self):
        self.manifest.write_text(json.dumps(self.values))

    def native_run(self, args, **kwargs):
        self.calls.append(args)
        output = 'not-found\n' if args[0] == 'systemctl' else self.native_version
        return subprocess.CompletedProcess(args, 0, output, '')

    def prepare(self):
        return prepare(self.runtime, self.manifest, self.root, self.native_run)

    def test_exact_embedded_bytes_modes_and_k0s_mtime_survive_repeat(self):
        first = self.prepare()
        paths = [self.root/'var/lib/k0s/bin'/name for name in COMPONENTS]
        inodes = [path.stat().st_ino for path in paths]
        for name, path in zip(COMPONENTS, paths):
            self.assertEqual(path.read_bytes(), ('embedded '+name).encode())
            self.assertEqual(path.stat().st_mtime_ns, self.worker.stat().st_mtime_ns)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o750)
        self.assertEqual(self.prepare(), first)
        self.assertEqual(inodes, [path.stat().st_ino for path in paths])
        self.assertEqual(sum(args[-1] == '--version' for args in self.calls), 1)
        self.assertFalse((self.root/'etc/k0s').exists())
        self.assertFalse((self.root/'var/lib/k0s/containerd').exists())

    def test_rehashed_alternate_payload_cannot_impersonate_embedded_runtime(self):
        with zipfile.ZipFile(self.runtime, 'w') as archive:
            for component in self.values['components']:
                body = b'replacement'
                archive.writestr(component['name'], body)
                component.update(bytes=len(body), sha256=hashlib.sha256(body).hexdigest())
        self.values['runtimeArchiveSHA256'] = hashlib.sha256(self.runtime.read_bytes()).hexdigest()
        self.save_manifest()
        with self.assertRaisesRegex(ValueError, 'embedded k0s'):
            self.prepare()
        self.assertFalse((self.metadata/'linux-runtime.intent.json').exists())

    def test_joined_machine_or_runtime_store_is_not_modified(self):
        for directory in ['etc/k0s', 'etc/kubernetes', 'var/lib/k0s/containerd']:
            with self.subTest(directory=directory):
                path = self.root/directory
                path.mkdir(parents=True)
                (path/'identity').write_text('retain')
                with self.assertRaises(ValueError): self.prepare()
                self.assertEqual((path/'identity').read_text(), 'retain')
                (path/'identity').unlink()
                path.rmdir()
        self.assertFalse((self.metadata/'linux-runtime.intent.json').exists())

    def test_loaded_kubernetes_service_prevents_staging(self):
        def loaded(args, **kwargs):
            return subprocess.CompletedProcess(args, 0, 'loaded\n', '')
        with self.assertRaisesRegex(ValueError, 'service'):
            prepare(self.runtime, self.manifest, self.root, loaded)
        self.assertFalse((self.root/'var/lib/k0s/bin').exists())

    def test_incomplete_operation_is_not_replayed(self):
        self.native_version = 'containerd github.com/containerd/containerd 1.7.28 fixture'
        with self.assertRaisesRegex(ValueError, 'containerd 2'):
            self.prepare()
        self.assertTrue((self.metadata/'linux-runtime.intent.json').exists())
        self.assertFalse((self.metadata/'linux-runtime.json').exists())
        self.native_version = 'containerd github.com/containerd/containerd/v2 2.3.2 fixture'
        with self.assertRaisesRegex(ValueError, 'do not replay'):
            self.prepare()

    def test_modified_prepared_file_is_rejected_without_repair(self):
        self.prepare()
        binary = self.root/'var/lib/k0s/bin/containerd'
        binary.write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'identity'):
            self.prepare()
        self.assertEqual(binary.read_bytes(), b'changed')

    def test_wrong_archive_hash_rejected_before_intent(self):
        self.runtime.write_bytes(b'wrong archive')
        with self.assertRaisesRegex(ValueError, 'SHA256'):
            self.prepare()
        self.assertFalse((self.metadata/'linux-runtime.intent.json').exists())


if __name__ == '__main__': unittest.main()
