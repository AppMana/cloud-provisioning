import io
import pathlib
import tarfile
import tempfile
import unittest

from derive_windows import write_binary_layer


class WindowsLayerTest(unittest.TestCase):
    def test_root_plugin_replaces_the_existing_entrypoint(self):
        # The pinned native HostProcess image places the plugin at the root.
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'layer.tar'
            write_binary_layer(path, b'plugin', 'device-plugin-wddm.exe')
            with tarfile.open(path) as layer:
                self.assertEqual(layer.getnames(), ['device-plugin-wddm.exe'])
                self.assertEqual(layer.extractfile('device-plugin-wddm.exe').read(), b'plugin')

    def test_nested_parents_are_present_before_the_binary(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)/'layer.tar'
            write_binary_layer(path, b'worker', 'worker/bin/worker.exe')
            with tarfile.open(path) as layer:
                entries = layer.getmembers()
                self.assertEqual([x.name for x in entries], ['worker', 'worker/bin', 'worker/bin/worker.exe'])
                self.assertTrue(all(x.isdir() for x in entries[:-1]))

    def test_invalid_paths_cannot_escape_the_layer(self):
        for name in ['', '/x.exe', '../x.exe', 'a/../x.exe', 'C:/x.exe', r'a\x.exe', 'a//x.exe']:
            with self.subTest(name=name), self.assertRaises(ValueError):
                write_binary_layer(io.BytesIO(), b'worker', name)


if __name__ == '__main__':
    unittest.main()
