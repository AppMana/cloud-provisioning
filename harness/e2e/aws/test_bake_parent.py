"""Parent-layer selection based on the native k0s AMI's root-disk shape."""
import copy
import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from image_policy import linux_builder_parent

SCRIPT = pathlib.Path(__file__).with_name('bake.py')
IMAGE = dict(ImageId='ami-layer', State='available', Public=False, OwnerId='123',
             Architecture='x86_64', VirtualizationType='hvm', RootDeviceType='ebs',
             RootDeviceName='/dev/sda1', Tags=[dict(Key='cloud-provisioning-test', Value='test')],
             BlockDeviceMappings=[dict(DeviceName='/dev/sda1', Ebs=dict(VolumeSize=20)),
                                  dict(DeviceName='/dev/sdb', VirtualName='ephemeral0'),
                                  dict(DeviceName='/dev/sdc', VirtualName='ephemeral1')])


class ParentLayer(unittest.TestCase):
    def exercise(self, image=None, existing=False):
        with tempfile.TemporaryDirectory() as directory:
            work = pathlib.Path(directory)
            state = dict(account='123', region='us-west-2', runID='test', baseAMIID='ami-base',
                         preparedAMIID='ami-default', amiID='ami-default', subnetID='subnet',
                         securityGroupID='sg', instanceProfileName='profile')
            if existing:
                state.update(builderInstanceID='i-original', builderParent=dict(imageID='ami-other'))
            (work/'resources.json').write_text(json.dumps(state))
            calls = []
            native = subprocess.run
            def run(args, **kwargs):
                if args[:2] == ['bash', '-n']:
                    return native(args, **kwargs)
                operation = args[4]
                inputs = json.loads(args[args.index('--cli-input-json')+1])
                calls.append((operation, inputs))
                if operation == 'get-caller-identity': result = {'Account': '123'}
                elif operation == 'describe-images': result = {'Images': [image or IMAGE]}
                elif operation == 'describe-instances':
                    result = {'Reservations': [{'Instances': [dict(InstanceId='i-original', State=dict(Name='running'))]}]}
                elif operation == 'run-instances': result = {'Instances': [{'InstanceId': 'i-new'}]}
                else: self.fail(operation)
                return subprocess.CompletedProcess(args, 0, json.dumps(result), '')
            error = None
            with patch.object(sys, 'argv', [str(SCRIPT), 'start', '--work-dir', str(work),
                                            '--base-image-id', 'ami-layer']), patch('subprocess.run', run):
                try: runpy.run_path(str(SCRIPT), run_name='__main__')
                except RuntimeError as caught: error = caught
            return calls, json.loads((work/'resources.json').read_text()), error

    def test_native_disk_shape_and_larger_parent_preserve_defaults(self):
        for size, device in [(20, '/dev/sda1'), (80, '/dev/xvda')]:
            image = copy.deepcopy(IMAGE)
            image['RootDeviceName'] = device
            image['BlockDeviceMappings'][0].update(DeviceName=device, Ebs=dict(VolumeSize=size))
            calls, state, error = self.exercise(image)
            self.assertIsNone(error)
            launch = next(inputs for op, inputs in calls if op == 'run-instances')
            self.assertEqual(launch['ImageId'], 'ami-layer')
            self.assertEqual(launch['BlockDeviceMappings'], [dict(DeviceName=device,
                Ebs=dict(VolumeSize=size, VolumeType='gp3', Encrypted=True, DeleteOnTermination=True))])
            self.assertEqual((state['baseAMIID'], state['amiID'], state['preparedAMIID']),
                             ('ami-base', 'ami-default', 'ami-default'))
            self.assertEqual(state['builderParent']['imageID'], 'ami-layer')

    def test_invalid_parent_cannot_launch(self):
        for change in [dict(Public=True), dict(Platform='windows'), dict(OwnerId='other'),
                       dict(Tags=[]), dict(State='pending'), dict(Architecture='arm64'),
                       dict(RootDeviceType='instance-store'), dict(VirtualizationType='paravirtual'),
                       dict(BlockDeviceMappings=[]), dict(RootDeviceName='/dev/missing')]:
            with self.subTest(change=change):
                calls, state, error = self.exercise(dict(IMAGE, **change))
                self.assertIsNotNone(error)
                self.assertNotIn('run-instances', [op for op, _ in calls])
                self.assertNotIn('builderInstanceID', state)

    def test_ambiguous_or_invalid_disk_inventory(self):
        for disks in [[dict(DeviceName='/dev/sda1', Ebs={})],
                      [dict(DeviceName='/dev/sda1', Ebs=dict(VolumeSize=True))],
                      [IMAGE['BlockDeviceMappings'][0]] * 2]:
            with self.assertRaises(RuntimeError):
                linux_builder_parent(lambda *a, **kw: {'Images': [dict(IMAGE, BlockDeviceMappings=disks)]},
                                     dict(account='123', runID='test'), 'ami-layer')

    def test_existing_builder_cannot_silently_change_parent(self):
        calls, state, error = self.exercise(existing=True)
        self.assertIsNotNone(error)
        self.assertEqual(state['builderInstanceID'], 'i-original')
        self.assertNotIn('run-instances', [op for op, _ in calls])


if __name__ == '__main__': unittest.main()
