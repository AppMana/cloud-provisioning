"""Exercise rendered CAPI objects at the CLI boundary without provisioning."""
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name('claim.py')

class ClaimTest(unittest.TestCase):
    def run_claim(self, version=None, image_state='available', volume=None, unprepared=False, gpu=False, instance_type=None, machine_template=None, cache=None, linux_image=None, authorized=True):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        root = pathlib.Path(temp.name)
        state = dict(runID='test-run', region='us-west-2', vpcID='vpc-owned',
                     subnetID='subnet-owned', securityGroupID='sg-owned',
                     instanceProfileName='profile', amiID='ami-linux',
                     windowsImageBuilds={year: dict(imageID='ami-'+year, imageState=image_state)
                                         for year in ['2022', '2025']})
        state['windowsGpuImageBuilds']={year:dict(imageID='ami-gpu-'+year,imageState=image_state) for year in ['2022','2025']}
        if cache is not None:
            for key in ['windowsImageBuilds','windowsGpuImageBuilds']:
                for image in state[key].values(): image.update(cache)
        if linux_image and authorized:
            state['authorizedLinuxAMIs'] = [linux_image]
        if unprepared:
            state['baseAMIID'] = state['amiID']
        (root/'resources.json').write_text(json.dumps(state))
        docker = root/'docker'
        docker.write_text('''#!/usr/bin/env python3
import json, os, pathlib, sys
root = pathlib.Path(os.environ['CLAIM_TEST_ROOT'])
if 'apply' in sys.argv:
 with (root/'objects.jsonl').open('a') as f: f.write(sys.stdin.read()+'\\n')
elif 'importedcontrolplane' in sys.argv:
 print(json.dumps({'status': {'initialization': {'controlPlaneInitialized': True}}}))
''')
        docker.chmod(0o700)
        args = ['python3', str(SCRIPT), '--work-dir', str(root), '--api-server', 'https://10.10.0.10:6443']
        if linux_image: args += ['--linux-image-id',linux_image]
        if version: args += ['--windows-version', version]
        if gpu: args += ['--variant','gpu']
        if instance_type: args += ['--instance-type',instance_type]
        if machine_template: args += ['--machine-template',machine_template]
        if volume is not None: args += ['--root-volume-gib', str(volume)]
        result = subprocess.run(args, capture_output=True, text=True,
                                env=dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'], CLAIM_TEST_ROOT=str(root)))
        objects = [json.loads(line) for line in (root/'objects.jsonl').read_text().splitlines()] if (root/'objects.jsonl').exists() else []
        return result, objects

    def test_explicit_linux_candidate_selects_its_template_without_changing_os(self):
        result, objects = self.run_claim(linux_image='ami-candidate')
        self.assertEqual(result.returncode,0,result.stderr)
        template = next(o for o in objects if o['kind']=='AWSMachineTemplate')['spec']['template']
        self.assertEqual(template['spec']['ami']['id'],'ami-candidate')
        self.assertEqual(template['metadata']['labels']['kubernetes.io/os'],'linux')

    def test_unapproved_candidate_or_windows_combination_has_no_kubernetes_writes(self):
        for args in [dict(linux_image='ami-candidate',authorized=False),dict(version='2022',linux_image='ami-candidate')]:
            result, objects = self.run_claim(**args)
            self.assertNotEqual(result.returncode,0)
            self.assertEqual(objects,[])

    def test_each_windows_image_selects_native_bootstrap(self):
        for year in ['2022', '2025']:
            with self.subTest(year=year):
                result, objects = self.run_claim(year)
                self.assertEqual(result.returncode, 0, result.stderr)
                template = next(o for o in objects if o['kind'] == 'AWSMachineTemplate')['spec']['template']
                self.assertEqual(template['metadata']['labels']['kubernetes.io/os'], 'windows')
                self.assertEqual(template['spec']['ami']['id'], 'ami-'+year)
                self.assertEqual(template['spec']['rootVolume']['size'], 50)
                self.assertFalse(template['spec']['cloudInit']['insecureSkipSecretsManager'])
                self.assertEqual(objects[-1]['kind'], 'ProvisionedNodeClaim')

    def test_linux_selection_retained(self):
        result, objects = self.run_claim()
        self.assertEqual(result.returncode, 0, result.stderr)
        template = next(o for o in objects if o['kind'] == 'AWSMachineTemplate')['spec']['template']
        self.assertEqual(template['metadata']['labels']['kubernetes.io/os'], 'linux')
        self.assertEqual(template['spec']['ami']['id'], 'ami-linux')
        self.assertEqual(template['spec']['rootVolume']['size'], 40)

    def test_new_template_revision_keeps_claim_name_for_all_guest_types(self):
        for version in [None,'2022','2025']:
            with self.subTest(version=version):
                result,objects=self.run_claim(version,machine_template='worker-image-v2')
                self.assertEqual(result.returncode,0,result.stderr)
                template=next(o for o in objects if o['kind']=='AWSMachineTemplate')
                claim=next(o for o in objects if o['kind']=='ProvisionedNodeClaim')
                self.assertEqual(template['metadata']['name'],'worker-image-v2')
                self.assertEqual(claim['metadata']['name'],'aws-remote1')
                self.assertEqual(claim['spec']['infrastructureRef']['name'],'worker-image-v2')

    def test_unprepared_linux_base_rejected_before_cluster_mutation(self):
        result, objects = self.run_claim(unprepared=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unprepared Ubuntu base AMI', result.stderr)
        self.assertEqual(objects, [])

    def test_windows_claim_does_not_depend_on_linux_preparation(self):
        result, objects = self.run_claim('2022', unprepared=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(objects[-1]['kind'], 'ProvisionedNodeClaim')

    def test_pending_image_rejected_before_cluster_mutation(self):
        result, objects = self.run_claim('2025', image_state='pending')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(objects, [])

    def test_alias_recipe_requires_verified_targets_before_cluster_mutation(self):
        for verified in [None,False,True]:
            cache={'cacheRecipe':{'schemaVersion':3,'dataDirectoryPermissions':'inherit-parent'},
                   'cacheVerification':{'passed':True,'checks':{'dataDirectoryInheritsPermissions':True,'exactImageTargets':verified}}}
            result,objects=self.run_claim('2025',cache=cache)
            if verified:
                self.assertEqual(result.returncode,0,result.stderr)
            else:
                self.assertNotEqual(result.returncode,0)
                self.assertEqual(objects,[])

    def test_cache_permissions_gate_precedes_any_cluster_mutation(self):
        for year in ['2022','2025']:
            for gpu in [False,True]:
                for verified in [False,True]:
                    with self.subTest(year=year,gpu=gpu,verified=verified):
                        cache={'cacheRecipe':{'schemaVersion':2 if verified else 1,
                                              'dataDirectoryPermissions':'inherit-parent'},
                               'cacheVerification':{'passed':True,'checks':{'dataDirectoryInheritsPermissions':verified}}}
                        result,objects=self.run_claim(year,gpu=gpu,instance_type='g6f.large',cache=cache)
                        if verified:
                            self.assertEqual(result.returncode,0,result.stderr)
                        else:
                            self.assertNotEqual(result.returncode,0)
                            self.assertIn('inherited-permissions',result.stderr)
                            self.assertEqual(objects,[])

    def test_captured_root_volume_sets_default_and_rejects_smaller_claim(self):
        for year in ['2022', '2025']:
            for gpu in [False, True]:
                args=dict(version=year, gpu=gpu, instance_type='g6f.large' if gpu else None,
                          cache={'rootVolumeGiB': 100})
                result, objects = self.run_claim(**args)
                self.assertEqual(result.returncode, 0, result.stderr)
                template=next(o for o in objects if o['kind']=='AWSMachineTemplate')
                self.assertEqual(template['spec']['template']['spec']['rootVolume']['size'],100)
                result, objects = self.run_claim(**args, volume=60)
                self.assertNotEqual(result.returncode,0)
                self.assertIn('at least 100',result.stderr)
                self.assertEqual(objects,[])
                result, objects = self.run_claim(**args, volume=120)
                self.assertEqual(result.returncode,0,result.stderr)

    def test_invalid_recorded_root_volume_prevents_mutations(self):
        for size in [True, '100', 0, -1]:
            result, objects = self.run_claim('2025',cache={'rootVolumeGiB':size})
            self.assertNotEqual(result.returncode,0)
            self.assertEqual(objects,[])

    def test_small_windows_volume_rejected_before_cluster_mutation(self):
        result, objects = self.run_claim('2022', volume=40)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(objects, [])

    def test_gpu_images_keep_native_windows_bootstrap(self):
        for year in ['2022','2025']:
            with self.subTest(year=year):
                result,objects=self.run_claim(year,gpu=True,instance_type='g6f.large')
                self.assertEqual(result.returncode,0,result.stderr)
                template=next(o for o in objects if o['kind']=='AWSMachineTemplate')['spec']['template']
                self.assertEqual(template['metadata']['labels']['kubernetes.io/os'],'windows')
                self.assertEqual(template['spec']['ami']['id'],'ami-gpu-'+year)
                self.assertEqual(template['spec']['instanceType'],'g6f.large')
                self.assertEqual(template['spec']['rootVolume']['size'],60)
                self.assertFalse(template['spec']['cloudInit']['insecureSkipSecretsManager'])

    def test_gpu_selection_requires_os_instance_and_large_enough_volume(self):
        for kw in [{'version':'2025'}, {'instance_type':'g6f.large'},
                   {'version':'2025','instance_type':'g6f.large','volume':50}]:
            with self.subTest(kw=kw):
                result,objects=self.run_claim(gpu=True,**kw)
                self.assertNotEqual(result.returncode,0)
                self.assertEqual(objects,[])

if __name__ == '__main__': unittest.main()
