import copy
import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from image_policy import owned_image

SCRIPT = pathlib.Path(__file__).with_name('bake.py')


class CandidateAuthorization(unittest.TestCase):
    def exercise(self, change=None):
        with tempfile.TemporaryDirectory() as directory:
            work = pathlib.Path(directory)
            state = dict(account='123', region='us-west-2', runID='test', amiID='ami-default',
                         preparedAMIID='ami-default', capaRoleName='role', capaRoleARN='arn:role')
            (work/'resources.json').write_text(json.dumps(state))
            image = dict(ImageId='ami-candidate', State='available', Public=False, OwnerId='123',
                         Tags=[{'Key':'cloud-provisioning-test','Value':'test'}])
            image.update(change or {})
            policy = {'Statement': [dict(Sid='UseApprovedLaunchResources', Effect='Allow',
                Action=['ec2:RunInstances'], Resource=['arn:default','arn:windows2022','arn:windows2025','arn:subnet'],
                Condition={'StringEquals':{'aws:RequestedRegion':'us-west-2'}}),
                dict(Sid='Unrelated', Effect='Allow', Action=['ec2:DescribeInstances'], Resource='*')]}
            original = copy.deepcopy(policy)
            changes = []
            def run(args, **kwargs):
                op = args[4]; inputs = json.loads(args[args.index('--cli-input-json')+1])
                if op == 'get-caller-identity': result = {'Account':'123'}
                elif op == 'describe-images': result = {'Images':[image]}
                elif op == 'get-role': result = {'Role':dict(Arn='arn:role',Tags=[{'Key':'cloud-provisioning-test','Value':'test'}])}
                elif op == 'get-role-policy': result = {'PolicyDocument':policy}
                elif op == 'put-role-policy': changes.append(json.loads(inputs['PolicyDocument'])); result = {}
                else: self.fail(op)
                return subprocess.CompletedProcess(args,0,json.dumps(result),'')
            error = None
            with patch.object(sys,'argv',[str(SCRIPT),'authorize','--work-dir',str(work),'--image-id','ami-candidate']), patch('subprocess.run',run):
                try: runpy.run_path(str(SCRIPT),run_name='__main__')
                except RuntimeError as caught: error = caught
            return original, changes, json.loads((work/'resources.json').read_text()), error

    def test_authorization_only_extends_live_launch_resources(self):
        original, changes, state, error = self.exercise()
        self.assertIsNone(error)
        expected = copy.deepcopy(original)
        expected['Statement'][0]['Resource'].append('arn:aws:ec2:us-west-2::image/ami-candidate')
        self.assertEqual(changes,[expected])
        self.assertEqual(state['amiID'],'ami-default')
        self.assertEqual(state['preparedAMIID'],'ami-default')
        self.assertEqual(state['authorizedLinuxAMIs'],['ami-candidate'])

    def test_unavailable_foreign_public_wrong_identity_or_windows_image_cannot_change_iam(self):
        for change in [dict(State='pending'),dict(OwnerId='other'),dict(Public=True),
                       dict(ImageId='ami-other'),dict(Platform='windows'),dict(Tags=[])]:
            with self.subTest(change=change):
                _, changes, state, error = self.exercise(change)
                self.assertIsNotNone(error)
                self.assertEqual(changes,[])
                self.assertNotIn('authorizedLinuxAMIs',state)

    def test_ambiguous_image_observation_is_rejected(self):
        with self.assertRaisesRegex(RuntimeError,'exactly one'):
            owned_image(lambda *args,**kwargs:{'Images':[]},{},'ami-test','linux')


if __name__=='__main__': unittest.main()
