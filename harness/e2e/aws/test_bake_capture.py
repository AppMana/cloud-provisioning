"""A host observation interruption must not replay an SSM guest operation."""
import hashlib
import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = pathlib.Path(__file__).with_name('bake.py')


class CaptureResume(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.work = pathlib.Path(directory.name)
        self.state = dict(account='123', region='us-west-2', runID='cldt-test',
                          builderInstanceID='i-builder', vpcID='vpc-test')
        self.save()
        self.calls = []
        self.result = dict(Status='Success', ResponseCode=0)
        self.stop_error = False
        self.send_error = False

    def save(self):
        (self.work/'resources.json').write_text(json.dumps(self.state))

    def run_aws(self, args, **kwargs):
        if args[:2] == ['bash', '-n']:
            return subprocess.CompletedProcess(args, 0, '', '')
        op = args[4]
        inputs = json.loads(args[args.index('--cli-input-json')+1])
        self.calls.append((op, inputs))
        if op == 'get-caller-identity':
            result = {'Account': '123'}
        elif op == 'describe-instances':
            result = {'Reservations': [{'Instances': [dict(
                InstanceId='i-builder', ImageId='ami-parent', VpcId='vpc-test', State={'Name': 'stopped'},
                Tags=[{'Key': 'cloud-provisioning-test', 'Value': 'cldt-test'}])]}]}
        elif op == 'send-command':
            if self.send_error:
                raise OSError('connection lost after submission')
            result = {'Command': {'CommandId': 'original-command'}}
        elif op == 'get-command-invocation':
            self.assertEqual(inputs['CommandId'], 'original-command')
            result = self.result
        elif op == 'stop-instances':
            if self.stop_error:
                raise OSError('host interrupted before stop response')
            result = {}
        elif op == 'create-image':
            result = {'ImageId': 'ami-candidate'}
        else:
            self.fail(op)
        return subprocess.CompletedProcess(args, 0, json.dumps(result), '')

    def invoke(self, timeout=False, replace=False, check=None):
        clock = iter([0, 631]) if timeout else None
        with patch.object(sys, 'argv', [str(SCRIPT), 'capture', '--work-dir', str(self.work)] + (['--replace-candidate'] if replace else []) + (['--capture-check', str(check)] if check else [])), \
                patch('subprocess.run', self.run_aws), patch('time.sleep'), \
                patch('time.monotonic', side_effect=(lambda: next(clock)) if timeout else None, return_value=0):
            try:
                runpy.run_path(str(SCRIPT), run_name='__main__')
            finally:
                self.state = json.loads((self.work/'resources.json').read_text())

    def count(self, op):
        return sum(name == op for name, _ in self.calls)

    def test_additional_check_is_bound_and_resumed_without_resubmission(self):
        check = self.work/'new-check.sh'
        check.write_text('echo independent-cache-check\n')
        original = 'echo original-build-verifier\n'
        (self.work/'additional-verify.sh').write_text(original)
        self.state['imageRecipe'] = dict(verifySHA256=hashlib.sha256(original.encode()).hexdigest())
        self.save()
        with self.assertRaisesRegex(RuntimeError, 'observation timed out'):
            self.invoke(timeout=True, check=check)
        check.write_text('echo changed-input-file\n')
        self.invoke()
        self.assertEqual(self.count('send-command'), 1)
        command = next(value['Parameters']['commands'][0] for op, value in self.calls if op == 'send-command')
        self.assertIn('independent-cache-check', command)
        self.assertIn('original-build-verifier', command)
        self.assertNotIn('changed-input-file', command)

    def test_cannot_add_check_after_capture_submission(self):
        with self.assertRaisesRegex(RuntimeError, 'observation timed out'):
            self.invoke(timeout=True)
        check = self.work/'late.sh'
        check.write_text('true\n')
        with self.assertRaisesRegex(RuntimeError, 'already begun'):
            self.invoke(check=check)
        self.assertEqual(self.count('send-command'), 1)

    def test_bound_check_tampering_prevents_resume(self):
        check = self.work/'check.sh'
        check.write_text('true\n')
        with self.assertRaisesRegex(RuntimeError, 'observation timed out'):
            self.invoke(timeout=True, check=check)
        (self.work/self.state['captureCheck']['file']).write_text('false\n')
        with self.assertRaisesRegex(RuntimeError, 'content changed'):
            self.invoke()
        self.assertEqual(self.count('create-image'), 0)

    def test_parent_identity_is_preserved_in_capture_receipt(self):
        self.state['builderParent'] = dict(imageID='ami-parent', rootDeviceName='/dev/sda1', rootVolumeGiB=20)
        self.save()
        self.invoke()
        self.assertEqual(self.state['builderVerification']['parent'], self.state['builderParent'])

    def test_wrong_parent_cannot_verify_or_capture(self):
        self.state['builderParent'] = dict(imageID='ami-other')
        self.save()
        with self.assertRaisesRegex(RuntimeError, 'parent differs'):
            self.invoke()
        self.assertEqual(self.count('send-command'), 0)
        self.assertEqual(self.count('create-image'), 0)

    def test_timeout_resumes_original_command_then_captures(self):
        with self.assertRaisesRegex(RuntimeError, 'observation timed out'):
            self.invoke(timeout=True)
        self.assertEqual(self.count('create-image'), 0)
        self.assertEqual(self.state['builderVerification']['status'], 'submitted')
        self.invoke()
        self.assertEqual(self.count('send-command'), 1)
        self.assertEqual(self.count('create-image'), 1)
        command = next(value['Parameters']['commands'][0] for op, value in self.calls if op == 'send-command')
        self.assertEqual(self.state['builderVerification']['commandSHA256'], hashlib.sha256(command.encode()).hexdigest())

    def test_success_before_stop_interruption_is_not_reexecuted(self):
        self.stop_error = True
        with self.assertRaises(OSError):
            self.invoke()
        self.stop_error = False
        self.invoke()
        self.assertEqual(self.count('send-command'), 1)
        self.assertEqual(self.count('get-command-invocation'), 1)
        self.assertEqual(self.count('create-image'), 1)

    def test_failed_original_command_cannot_capture_or_be_replaced(self):
        for result in [dict(Status='Failed', ResponseCode=1), dict(Status='Success', ResponseCode=1)]:
            with self.subTest(result=result):
                self.result = result
                for _ in range(2):
                    with self.assertRaisesRegex(RuntimeError, 'original image preparation'):
                        self.invoke()
                self.assertEqual(self.count('send-command'), 1)
                self.assertEqual(self.count('create-image'), 0)

    def test_uncertain_submission_is_not_replayed(self):
        self.send_error = True
        with self.assertRaises(OSError):
            self.invoke()
        with self.assertRaisesRegex(RuntimeError, 'submission outcome is unknown'):
            self.invoke()
        self.assertEqual(self.count('send-command'), 1)
        self.assertEqual(self.count('stop-instances'), 0)

    def test_changed_instance_or_recipe_cannot_reuse_original_command(self):
        with self.assertRaises(RuntimeError):
            self.invoke(timeout=True)
        for field in ['instanceID', 'commandSHA256']:
            original = self.state['builderVerification'][field]
            self.state['builderVerification'][field] = 'different'
            self.save()
            with self.assertRaisesRegex(RuntimeError, 'different instance or recipe'):
                self.invoke()
            self.state['builderVerification'][field] = original
        self.assertEqual(self.count('send-command'), 1)
        self.assertEqual(self.count('create-image'), 0)

    def test_replacement_verifies_updated_builder_in_a_new_command(self):
        self.invoke()
        self.invoke(replace=True)
        self.assertEqual(self.count('send-command'), 2)
        self.assertEqual(self.count('create-image'), 2)
        self.assertEqual(len(self.state['builderVerificationHistory']), 1)
        self.assertEqual(self.state['builderVerificationHistory'][0]['capturedImageID'], 'ami-candidate')

    def test_unbound_legacy_command_requires_reconciliation(self):
        self.state['builderCommandID'] = 'legacy-command'
        self.save()
        with self.assertRaisesRegex(RuntimeError, 'legacy builder command'):
            self.invoke()
        self.assertEqual(self.count('send-command'), 0)


if __name__ == '__main__':
    unittest.main()
