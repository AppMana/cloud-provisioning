import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from image_policy import owned_image


ROOT = Path(__file__).parent


class ArchitectureTests(unittest.TestCase):
    def test_bootstrap_selects_architecture_before_creating_infrastructure(self):
        for architecture, name in [('x86_64', 'amd64'), ('arm64', 'arm64')]:
            with self.subTest(architecture=architecture), tempfile.TemporaryDirectory() as directory:
                calls = []
                def run(args, **kwargs):
                    calls.append(args)
                    if 'get-caller-identity' in args:
                        result = {'Account': '123', 'Arn': 'arn:aws:iam::123:user/setup'}
                    elif 'describe-images' in args:
                        result = {'Images': [{'ImageId': 'ami-base', 'CreationDate': '2026-01-01T00:00:00Z'}]}
                    else:
                        raise RuntimeError('stop before infrastructure mutation')
                    return subprocess.CompletedProcess(args, 0, json.dumps(result), '')
                argv = ['bootstrap.py', '--work-dir', directory, '--run-id', 'cldt-arch', '--architecture', architecture]
                with patch.object(sys, 'argv', argv), patch('subprocess.run', run):
                    with self.assertRaisesRegex(RuntimeError, 'stop before infrastructure mutation'):
                        runpy.run_path(str(ROOT / 'bootstrap.py'), run_name='__main__')
                query = next(c for c in calls if 'describe-images' in c)
                self.assertIn('Name=architecture,Values=' + architecture, query)
                self.assertTrue(any('noble-24.04-' + name + '-server-' in x for x in query))
                self.assertIn('099720109477', query)
                state = json.loads((Path(directory) / 'resources.json').read_text())
                self.assertEqual(state['architecture'], architecture)

    def test_authorization_rejects_cross_architecture_images(self):
        image = dict(ImageId='ami-owned', State='available', Public=False, OwnerId='123',
                     Architecture='x86_64', Tags=[dict(Key='cloud-provisioning-test', Value='cldt-test')])
        state = dict(account='123', runID='cldt-test', architecture='arm64')
        with self.assertRaisesRegex(RuntimeError, 'architecture differs'):
            owned_image(lambda *a, **kw: {'Images': [image]}, state, 'ami-owned', 'linux')
        image['Architecture'] = 'arm64'
        self.assertEqual(owned_image(lambda *a, **kw: {'Images': [image]}, state, 'ami-owned', 'linux'), image)

    def test_recipe_selects_verified_installer_and_rejects_unsupported_architecture(self):
        prefix = (ROOT / 'image-prepare.sh').read_text().split('apt-get update', 1)[0]
        for arch, expected in [('x86_64', 'd1d0c36d2582925e52022d8675e7fdaed8bb871af97ce2ff7c69e8bd81b123a0'),
                               ('aarch64', 'e70dca4d9109efd807e173782d7024f524f23b45d24645ea32f44a175e6eb19e'),
                               ('riscv64', None)]:
            with self.subTest(architecture=arch), tempfile.TemporaryDirectory() as directory:
                uname = Path(directory) / 'uname'
                uname.write_text('#!/bin/sh\necho ' + arch + '\n')
                uname.chmod(0o700)
                result = subprocess.run(['bash', '-c', prefix + '\nprintf "%s %s" "$awscli_arch" "$awscli_sha256"'],
                    env=dict(os.environ, PATH=directory + ':' + os.environ['PATH']), capture_output=True, text=True)
                if expected:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, arch + ' ' + expected)
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(result.stdout, '')


if __name__ == '__main__':
    unittest.main()
