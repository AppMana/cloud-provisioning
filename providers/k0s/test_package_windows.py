import hashlib
import importlib.util
import pathlib
import struct
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location('package_windows', pathlib.Path(__file__).with_name('package-windows.py'))
package_windows = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package_windows)


def pe():
    data = bytearray(128)
    data[:2] = b'MZ'
    struct.pack_into('<I', data, 60, 64)
    data[64:68] = b'PE\0\0'
    struct.pack_into('<H', data, 68, 0x8664)
    return bytes(data)


class WindowsPackageTest(unittest.TestCase):
    def test_preserves_upstream_runtime_bytes_and_rejects_wrong_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            bare, upstream = root/'bare.exe', root/'upstream.exe'
            bare.write_bytes(pe() + b'new-worker')
            upstream.write_bytes(pe() + b'old-worker')
            with zipfile.ZipFile(upstream, 'a', compression=zipfile.ZIP_DEFLATED) as archive:
                for name in ('containerd.exe', 'containerd-shim-runhcs-v1.exe', 'kubelet.exe'):
                    archive.writestr(name, b'original-runtime-' + name.encode())
            sha = lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
            receipt = package_windows.package(bare, sha(bare), upstream, sha(upstream), root/'out')
            self.assertEqual(receipt['binarySHA256'], sha(root/'out/k0s.exe'))
            with zipfile.ZipFile(upstream) as original, zipfile.ZipFile(root/'out/k0s.exe') as built:
                for name in original.namelist():
                    self.assertEqual(original.read(name), built.read(name))
            with self.assertRaises(FileExistsError):
                package_windows.package(bare, sha(bare), upstream, sha(upstream), root/'out')
            with self.assertRaisesRegex(ValueError, 'checksum'):
                package_windows.package(bare, '0'*64, upstream, sha(upstream), root/'wrong')
            with self.assertRaisesRegex(ValueError, 'already has'):
                package_windows.package(upstream, sha(upstream), upstream, sha(upstream), root/'double')
            with zipfile.ZipFile(upstream, 'a') as archive:
                archive.writestr('../unexpected.exe', b'unsafe')
            with self.assertRaisesRegex(ValueError, 'inventory'):
                package_windows.package(bare, sha(bare), upstream, sha(upstream), root/'unsafe')

    def test_rejects_non_windows_and_wrong_architecture(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'binary'
            for data in (b'ELF', pe()[:68] + b'\x4c\x01' + pe()[70:], pe()[:64]):
                path.write_bytes(data)
                with self.assertRaises(ValueError):
                    package_windows.verified_pe(path, hashlib.sha256(data).hexdigest())


if __name__ == '__main__':
    unittest.main()
