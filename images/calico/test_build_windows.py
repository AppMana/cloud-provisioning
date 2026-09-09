import hashlib
import importlib.util
import pathlib
import struct
import tempfile
import tarfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('calico_image', pathlib.Path(__file__).with_name('build-windows.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class BinaryContractTest(unittest.TestCase):
    def test_hash_and_native_architecture_are_both_required(self):
        raw=bytearray(128);raw[:2]=b'MZ';struct.pack_into('<I',raw,60,64);raw[64:68]=b'PE\0\0';struct.pack_into('<H',raw,68,0x8664)
        with tempfile.TemporaryDirectory() as directory:
            path=pathlib.Path(directory)/'calico-node.exe';path.write_bytes(raw)
            digest=hashlib.sha256(raw).hexdigest()
            self.assertEqual(module.checked_binary(path,digest),raw)
            with self.assertRaisesRegex(ValueError,'SHA-256'):
                module.checked_binary(path,'0'*64)
            for damaged in [b'not a PE', raw[:65], raw[:68]+b'\x4c\x01'+raw[70:]]:
                path.write_bytes(damaged)
                with self.assertRaisesRegex(ValueError,'executable'):
                    module.checked_binary(path,hashlib.sha256(damaged).hexdigest())

    def test_layer_has_parent_before_file_for_windows_extraction(self):
        # Native containerd extraction failed when only the base had this directory.
        with tempfile.TemporaryDirectory() as directory:
            path=pathlib.Path(directory)/'layer.tar'
            module.write_binary_layer(path,b'payload')
            with tarfile.open(path) as tar:
                entries=tar.getmembers()
                self.assertEqual([e.name.rstrip('/') for e in entries],['CalicoWindows','CalicoWindows/calico-node.exe'])
                self.assertTrue(entries[0].isdir())
                self.assertEqual(tar.extractfile(entries[1]).read(),b'payload')

    def test_mutable_base_rejected_before_network_or_output(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(module,'run') as run:
            output=pathlib.Path(directory)/'output'
            with self.assertRaisesRegex(ValueError,'pin'):
                module.build('calico/node-windows:v3.32.0',pathlib.Path('unused'),'unused',output)
            run.assert_not_called()
            self.assertFalse(output.exists())

if __name__=='__main__': unittest.main()
