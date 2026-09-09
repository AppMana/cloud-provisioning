import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).parent

class CacheImageSetTests(unittest.TestCase):
    def test_shared_set_preserves_os_specific_recipe_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            base = pathlib.Path(directory)
            runtime = base / 'runtime.json'
            runtime.write_text(json.dumps({'schemaVersion': 1, 'workerSHA256': 'a'*64,
                'components': [{'name': 'containerd.exe', 'sha256': 'b'*64},
                               {'name': 'containerd-shim-runhcs-v1.exe', 'sha256': 'c'*64}]}))
            image_set = ROOT / 'candidates/k0s-1.36-calico-3.32.json'
            outputs = []
            for year in ['2022', '2025']:
                output = base / (year + '.json')
                subprocess.run([sys.executable, str(ROOT/'cache.py'), '--runtime', str(runtime),
                    '--windows-version', year, '--image-file', str(image_set), '--output', str(output)],
                    check=True, capture_output=True, text=True)
                outputs.append(json.loads(output.read_text()))
            self.assertEqual(outputs[0]['images'], sorted(json.loads(image_set.read_text())))
            self.assertEqual(outputs[0]['images'], outputs[1]['images'])
            self.assertNotEqual(outputs[0]['sha256'], outputs[1]['sha256'])
            self.assertEqual([x['windowsBuild'] for x in outputs], [20348, 26100])

if __name__ == '__main__':
    unittest.main()
