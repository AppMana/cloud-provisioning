"""A Linux promotion must preserve Windows launch permissions in a mixed run."""
import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT=pathlib.Path(__file__).with_name('bake.py')

class MixedImagePromotion(unittest.TestCase):
    def promote(self, windows_owned=True, linux_public=False, extra_linux=False):
        with tempfile.TemporaryDirectory() as tmp:
            work=pathlib.Path(tmp)
            state=dict(account='123',region='us-west-2',runID='cldt-test',candidateAMIID='ami-linux-new',
                       builderInstanceID='i-builder',capaRoleName='cldt-capa',capaRoleARN='arn:role',
                       authorizedWindowsAMIs=['ami-2022','ami-2025'],retiredImages=[{'id':'ami-linux-new','snapshots':[]}])
            if extra_linux:state['authorizedLinuxAMIs']=['ami-linux-candidate']
            (work/'resources.json').write_text(json.dumps(state))
            (work/'iam-values.json').write_text(json.dumps({'AMI_ID':'ami-linux-old'}))
            mutations=[]
            def run(args,**kw):
                operation=args[4];inputs=json.loads(args[args.index('--cli-input-json')+1])
                if operation=='get-caller-identity': result={'Account':'123'}
                elif operation=='describe-images':
                    result={'Images':[dict(ImageId=i,State='available',Public=linux_public if i=='ami-linux-new' else False,
                                           OwnerId='123',Platform='windows' if '202' in i else 'linux',
                                           Tags=[{'Key':'cloud-provisioning-test','Value':'cldt-test' if windows_owned or i=='ami-linux-new' else 'other'}],
                                           BlockDeviceMappings=[{'Ebs':{'SnapshotId':'snap-test'}}]) for i in inputs['ImageIds']]}
                elif operation=='get-role':result={'Role':{'Arn':'arn:role','Tags':[{'Key':'cloud-provisioning-test','Value':'cldt-test'}]}}
                elif operation in ['put-role-policy','terminate-instances']:mutations.append((operation,inputs));result={}
                else: self.fail(operation)
                return subprocess.CompletedProcess(args,0,json.dumps(result),'')
            policy={'Statement':[{'Sid':'UseApprovedLaunchResources','Resource':['arn:aws:ec2:us-west-2::image/ami-linux-new','arn:subnet']}]}
            error=None
            with patch.object(sys,'argv',[str(SCRIPT),'promote','--work-dir',str(work)]),patch('subprocess.run',run),patch('subprocess.check_output',return_value=json.dumps(policy)):
                try:runpy.run_path(str(SCRIPT),run_name='__main__')
                except RuntimeError as e:error=e
            return mutations,error

    def test_linux_promotion_retains_both_windows_images(self):
        mutations,error=self.promote();self.assertIsNone(error)
        resources=json.loads(mutations[0][1]['PolicyDocument'])['Statement'][0]['Resource']
        self.assertEqual(set(resources),{'arn:subnet',*[f'arn:aws:ec2:us-west-2::image/{i}' for i in ['ami-linux-new','ami-2022','ami-2025']]})

    def test_linux_promotion_retains_additional_linux_candidates(self):
        mutations,error=self.promote(extra_linux=True)
        self.assertIsNone(error)
        resources=json.loads(mutations[0][1]['PolicyDocument'])['Statement'][0]['Resource']
        self.assertIn('arn:aws:ec2:us-west-2::image/ami-linux-candidate',resources)

    def test_foreign_windows_image_prevents_policy_change(self):
        mutations,error=self.promote(windows_owned=False)
        self.assertIsNotNone(error);self.assertEqual(mutations,[])

    def test_public_linux_candidate_is_not_promoted(self):
        mutations,error=self.promote(linux_public=True)
        self.assertIsNotNone(error);self.assertEqual(mutations,[])

class BuilderResume(unittest.TestCase):
    def exercise(self, error=None, invalid_recipe=False, previous_recipe=False):
        with tempfile.TemporaryDirectory() as tmp:
            work=pathlib.Path(tmp)
            state=dict(account='123',region='us-west-2',runID='cldt-test',builderInstanceID='i-old',
                       builderCommandID='old-command',builderVerification={'instanceID':'i-old','commandID':'old-command'},
                       baseAMIID='ami-base',subnetID='subnet-test',securityGroupID='sg-test',instanceProfileName='profile')
            if previous_recipe:
                state['imageRecipe'] = {'prepareSHA256': 'old-prepare', 'verifySHA256': 'old-verify'}
                (work/'additional-prepare.sh').write_text('echo old-GPU-preparation\n')
                (work/'additional-verify.sh').write_text('echo old-GPU-verification\n')
            (work/'resources.json').write_text(json.dumps(state));calls=[]
            native_run = subprocess.run
            def run(args,**kw):
                if args[:2] == ['bash', '-n']:
                    return native_run(args, **kw)
                op=args[4];calls.append(op)
                if op=='get-caller-identity':result={'Account':'123'}
                elif op=='describe-instances':
                    if error:return subprocess.CompletedProcess(args,1,'',error)
                    result={'Reservations':[{'Instances':[]}]}
                elif op=='run-instances':
                    inputs=json.loads(args[args.index('--cli-input-json')+1])
                    self.assertNotIn('old-GPU',inputs['UserData'])
                    result={'Instances':[{'InstanceId':'i-new'}]}
                else:self.fail(op)
                return subprocess.CompletedProcess(args,0,json.dumps(result),'')
            argv=[str(SCRIPT),'start','--work-dir',str(work)]
            if invalid_recipe:
                (work/'prepare.sh').write_text((SCRIPT.parent/'testdata/linux-recipe-heredoc.sh').read_text())
                (work/'verify.sh').write_text('true\n')
                argv += ['--prepare-script',str(work/'prepare.sh'),'--verify-script',str(work/'verify.sh')]
            with patch.object(sys,'argv',argv),patch('subprocess.run',run):
                if error and 'InvalidInstanceID.NotFound' not in error:
                    with self.assertRaises(RuntimeError):runpy.run_path(str(SCRIPT),run_name='__main__')
                elif invalid_recipe:
                    with self.assertRaisesRegex(ValueError, 'parser warnings'):
                        runpy.run_path(str(SCRIPT),run_name='__main__')
                else:runpy.run_path(str(SCRIPT),run_name='__main__')
            return calls,json.loads((work/'resources.json').read_text())

    def test_absent_recorded_builder_does_not_block_a_new_build(self):
        for error in [None,'InvalidInstanceID.NotFound']:
            calls,state=self.exercise(error)
            self.assertEqual(calls.count('run-instances'),1)
            self.assertEqual(state['builderInstanceID'],'i-new')
            self.assertEqual(state['imageBuilderHistory'],[{'instanceID':'i-old','observedState':'not-found',
                'builderCommandID':'old-command','builderVerification':{'instanceID':'i-old','commandID':'old-command'}}])
            self.assertNotIn('builderCommandID',state)
            self.assertNotIn('builderVerification',state)

    def test_native_recipe_warning_prevents_vm_launch(self):
        calls,state=self.exercise(invalid_recipe=True)
        self.assertNotIn('run-instances',calls)
        self.assertNotIn('builderInstanceID',state)

    def test_failed_observation_is_not_treated_as_absence(self):
        calls,state=self.exercise('AccessDenied')
        self.assertNotIn('run-instances',calls)
        self.assertEqual(state['builderInstanceID'],'i-old')

    def test_fresh_bootstrap_only_builder_does_not_inherit_gpu_verifier(self):
        calls,state=self.exercise(previous_recipe=True)
        self.assertEqual(calls.count('run-instances'),1)
        self.assertNotIn('imageRecipe',state)
        self.assertEqual(state['imageBuilderHistory'][-1]['imageRecipe'],
                         {'prepareSHA256':'old-prepare','verifySHA256':'old-verify'})


if __name__=='__main__':unittest.main()
