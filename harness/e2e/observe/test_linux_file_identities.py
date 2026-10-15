import hashlib
import os
import pathlib
import tempfile
import unittest
from unittest import mock

from linux_cache_native import file_identities


class RuntimeFileIdentities(unittest.TestCase):
    def test_replacement_with_same_bytes_and_mtime_changes_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'runtime'
            path.write_bytes(b'original runtime')
            before = file_identities([path])[str(path)]
            replacement = path.with_suffix('.new')
            replacement.write_bytes(path.read_bytes())
            os.utime(replacement, ns=(before['mtimeNs'], before['mtimeNs']))
            replacement.replace(path)
            after = file_identities([path])[str(path)]
            self.assertEqual(before['sha256'], after['sha256'])
            self.assertEqual(before['mtimeNs'], after['mtimeNs'])
            self.assertNotEqual(before['inode'], after['inode'])

    def test_replacement_during_hash_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'runtime'
            path.write_bytes(b'original')
            digest = hashlib.file_digest

            def replace(stream, algorithm):
                result = digest(stream, algorithm)
                temporary = path.with_suffix('.new')
                temporary.write_bytes(b'replacement')
                temporary.replace(path)
                return result

            with mock.patch('linux_cache_native.hashlib.file_digest', side_effect=replace):
                with self.assertRaisesRegex(ValueError, 'changed during observation'):
                    file_identities([path])

    def test_symlink_and_fifo_are_rejected_without_reading(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            target = root/'runtime'
            target.write_bytes(b'original')
            link = root/'link'
            link.symlink_to(target)
            fifo = root/'fifo'
            os.mkfifo(fifo)
            with self.assertRaises(OSError):
                file_identities([link])
            with self.assertRaisesRegex(ValueError, 'regular runtime file'):
                file_identities([fifo])


if __name__ == '__main__':
    unittest.main()
