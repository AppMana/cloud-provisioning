import copy
import pathlib
import tempfile
import unittest

from linux_transfer_cleanup import cleanup


class TransferCleanup(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = pathlib.Path(self.directory.name)
        self.proc = self.root/'proc'
        self.proc.mkdir()
        self.instance = 'i-001bd2820cad55bec'
        command = '5387353e-914a-4ca9-8101-0baa55bb4612'
        relative = f'orchestration/{command}/awsrunShellScript/0.awsrunShellScript/_script.sh'
        self.path = self.root/self.instance/'document'/relative
        self.path.parent.mkdir(parents=True)
        self.path.write_bytes(b'X-Amz-Signature='+b'0'*64)
        self.manifest = dict(instanceID=self.instance, commands=[dict(
            CommandId=command, InstanceId=self.instance, Status='Success', ResponseCode=0, path=relative)])

    def test_completed_script_removed_with_hash_receipt_and_other_history_preserved(self):
        other = self.path.with_name('stdout')
        other.write_text('retained')
        result = cleanup(self.manifest, self.root, self.proc)
        self.assertEqual(len(result['removed']), 1)
        self.assertFalse(self.path.exists())
        self.assertEqual(other.read_text(), 'retained')

    def test_running_process_overrides_successful_service_receipt(self):
        process = self.proc/'123'
        process.mkdir()
        (process/'cmdline').write_bytes(b'bash\0'+str(self.path).encode()+b'\0')
        with self.assertRaisesRegex(ValueError, 'guest process'):
            cleanup(self.manifest, self.root, self.proc)
        self.assertTrue(self.path.exists())

    def test_wrong_instance_status_or_path_cannot_delete_history(self):
        for key, value in [('InstanceId', 'i-00000000000000000'), ('Status', 'InProgress'),
                           ('ResponseCode', False), ('path', '../../unrelated')]:
            manifest = copy.deepcopy(self.manifest)
            manifest['commands'][0][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                cleanup(manifest, self.root, self.proc)
            self.assertTrue(self.path.exists())

    def test_symlink_is_not_followed(self):
        target = self.path.with_name('target')
        self.path.rename(target)
        self.path.symlink_to(target)
        with self.assertRaisesRegex(ValueError, 'symbolic'):
            cleanup(self.manifest, self.root, self.proc)
        self.assertTrue(target.exists())


if __name__ == '__main__':
    unittest.main()
