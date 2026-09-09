import json
import os
import pathlib
import subprocess
import tempfile
import unittest
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[3]))

SCRIPT=pathlib.Path(__file__).with_name('windows_image.py')

class WindowsImageCapture(unittest.TestCase):
    def run_capture(self, instance_state='stopped', owned=True, gpu=False, evidence=None, worker_sha='b'*64, cache=False, cache_permissions=True):
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        root=pathlib.Path(temp.name)
        state={'account':'123','region':'us-west-2','runID':'owned-run','vpcID':'vpc-owned',
               'windowsProbeInstances':{y:{'instanceID':'i-'+y} for y in ['2022','2025']},
               'windowsImageBuilds':{y:{'preparedAt':'now','generalizeRequestedAt':'now','tunnelSHA256':'a'*64,'workerSHA256':worker_sha} for y in ['2022','2025']}}
        if gpu:
            state['windowsImageBuilds']['2025']['imageID']='ami-cpu-2025'
            state['windowsGpuBuilders']={'2025':{'instanceID':'i-2025','baseImageID':'ami-cpu-2025'}}
            state['windowsGpuImageBuilds']={'2025':{k:v for k,v in dict(state['windowsImageBuilds']['2025'],gpu=evidence).items() if k!='imageID'}}
        if cache:
            from images.windows.cache import make_recipe
            fixtures=SCRIPT.parent.parent/'observe/testdata'
            runtime=json.loads((fixtures/'windows-cache-runtime.json').read_text())
            snapshot=json.loads((fixtures/'windows-cache-ready.json').read_text())
            for year,record in state['windowsImageBuilds'].items():
                record['workerSHA256']=runtime['workerSHA256']
                record['cacheRecipe']=make_recipe(runtime,snapshot['registeredImages'],year)
                record['requestedCacheRecipe']=record['cacheRecipe']
                record['cacheVerification']={'passed':True,'checks':{'dataDirectoryInheritsPermissions':cache_permissions}}
        (root/'resources.json').write_text(json.dumps(state))
        aws=root/'aws'
        aws.write_text('''#!/usr/bin/env python3
import json,os,pathlib,sys
service,operation=sys.argv[3:5]
root=pathlib.Path(os.environ['IMAGE_TEST_ROOT'])
inputs=json.loads(sys.argv[sys.argv.index('--cli-input-json')+1])
if service=='sts': print(json.dumps({'Account':'123'}))
elif operation=='describe-instances':
 print(json.dumps({'Reservations':[{'Instances':[{'InstanceId':'i-'+y,'VpcId':os.environ['IMAGE_TEST_VPC'],'Platform':'windows','NetworkInterfaces':[{}],'State':{'Name':os.environ['IMAGE_TEST_STATE']},'Tags':[{'Key':'cloud-provisioning-test','Value':'owned-run'}]} for y in ['2022','2025']]}]}))
elif operation=='create-image':
 with (root/'creates.jsonl').open('a') as f:f.write(json.dumps(inputs)+'\\n')
 print(json.dumps({'ImageId':'ami-'+inputs['InstanceId'][2:]}))
else:sys.exit(2)
''');aws.chmod(0o700)
        env=dict(os.environ,PATH=str(root)+os.pathsep+os.environ['PATH'],IMAGE_TEST_ROOT=str(root),IMAGE_TEST_VPC='vpc-owned' if owned else 'vpc-other',IMAGE_TEST_STATE=instance_state)
        result=subprocess.run(['python3',str(SCRIPT),'--work-dir',str(root),'--awsnode','/unused','--phase','capture']+(['--variant','gpu','--windows-version','2025'] if gpu else []),env=env,capture_output=True,text=True)
        return root,result

    def test_capture_records_both_images_for_cleanup(self):
        root,result=self.run_capture()
        self.assertEqual(result.returncode,0,result.stderr)
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual({x['id'] for x in state['retiredImages']},{'ami-2022','ami-2025'})
        creates=[json.loads(x) for x in (root/'creates.jsonl').read_text().splitlines()]
        for request in creates:
            self.assertTrue(request['NoReboot'])
            self.assertEqual({t['ResourceType'] for t in request['TagSpecifications']},{'image','snapshot'})
            for resource in request['TagSpecifications']:
                self.assertIn({'Key':'cloud-provisioning-test','Value':'owned-run'},resource['Tags'])

    def test_running_builder_is_not_captured(self):
        root,result=self.run_capture(instance_state='running')
        self.assertNotEqual(result.returncode,0)
        self.assertFalse((root/'creates.jsonl').exists())

    def test_worker_only_revision_gets_distinct_capture_names(self):
        names=[]
        for sha in ['b'*64,'c'*64]:
            root,result=self.run_capture(worker_sha=sha)
            self.assertEqual(result.returncode,0,result.stderr)
            names.append({json.loads(line)['Name'] for line in (root/'creates.jsonl').read_text().splitlines()})
        self.assertTrue(names[0].isdisjoint(names[1]))

    def test_capture_without_verified_worker_identity_is_rejected(self):
        root,result=self.run_capture(worker_sha='')
        self.assertNotEqual(result.returncode,0)
        self.assertFalse((root/'creates.jsonl').exists())

    def test_legacy_cache_readiness_does_not_qualify_directory_permissions(self):
        root,result=self.run_capture(cache=True,cache_permissions=None)
        self.assertNotEqual(result.returncode,0)
        self.assertIn('native data-directory permission evidence',result.stderr)
        self.assertFalse((root/'creates.jsonl').exists())

    def test_capture_name_includes_prepared_cache_recipe_identity(self):
        root,result=self.run_capture(cache=True)
        self.assertEqual(result.returncode,0,result.stderr)
        state=json.loads((root/'resources.json').read_text())
        for line in (root/'creates.jsonl').read_text().splitlines():
            request=json.loads(line)
            year=request['InstanceId'].removeprefix('i-')
            self.assertTrue(request['Name'].endswith('-cache-'+state['windowsImageBuilds'][year]['cacheRecipe']['sha256'][:12]))

    def test_foreign_vpc_is_not_captured(self):
        root,result=self.run_capture(owned=False)
        self.assertNotEqual(result.returncode,0)
        self.assertFalse((root/'creates.jsonl').exists())

    def test_gpu_capture_retains_cpu_candidates_and_driver_identity(self):
        evidence={'windowsBuild':26100,'verifiedAfterReboot':True,
                  'licenseScope':'aws','licenseSource':'https://docs.aws.amazon.com/',
                  'bootTime':'2026-09-07T16:28:56.5000000Z','packageSHA256':'b'*64,
                  'driverVersion':'582.53','gpuCount':1,
                  'gpus':[{'driverVersion':'582.53','driverModel':'WDDM','name':'NVIDIA L4-3Q'}],
                  'workloadValidated':False,'licenseActivationValidated':False}
        root,result=self.run_capture(gpu=True,evidence=evidence)
        self.assertEqual(result.returncode,0,result.stderr)
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['windowsImageBuilds']['2025']['imageID'],'ami-cpu-2025')
        self.assertEqual(state['windowsGpuImageBuilds']['2025']['imageID'],'ami-2025')
        self.assertEqual(state['windowsGpuImageBuilds']['2025']['gpu'],evidence)
        creates=[json.loads(x) for x in (root/'creates.jsonl').read_text().splitlines()]
        self.assertEqual(len(creates),1)
        self.assertIn('-gpu-'+('b'*12),creates[0]['Name'])

    def test_gpu_capture_without_reboot_evidence_is_rejected(self):
        root,result=self.run_capture(gpu=True)
        self.assertNotEqual(result.returncode,0)
        self.assertFalse((root/'creates.jsonl').exists())


class WindowsImageAuthorization(unittest.TestCase):
    def authorize(self, image_state='available', owned=True, gpu=False, extra_linux=False):
        import runpy
        import sys
        from unittest.mock import patch
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        root=pathlib.Path(temp.name)
        state={'account':'123','region':'us-west-2','runID':'cldt-test',
               'capaRoleName':'cldt-test-capa','capaRoleARN':'arn:role',
               'windowsImageBuilds':{y:{'imageID':'ami-'+y} for y in ['2022','2025']}}
        if extra_linux:state['authorizedLinuxAMIs']=['ami-linux-candidate']
        (root/'resources.json').write_text(json.dumps(state))
        if gpu:
            state['authorizedWindowsAMIs']=['ami-2022','ami-2025']
            state['windowsGpuImageBuilds']={'2025':{'imageID':'ami-gpu-2025'}}
            (root/'resources.json').write_text(json.dumps(state))
        policy={'Statement':[{'Sid':'UseApprovedLaunchResources','Resource':['arn:aws:ec2:us-west-2::image/ami-linux','arn:subnet']}]}
        mutations=[]
        def run(args,**kw):
            operation=args[4]
            inputs=json.loads(args[args.index('--cli-input-json')+1])
            if operation=='get-caller-identity': result={'Account':'123'}
            elif operation=='describe-images':
                result={'Images':[{'ImageId':inputs['ImageIds'][0],'State':image_state,'Public':False,'OwnerId':'123','Platform':'linux' if inputs['ImageIds'][0]=='ami-linux-candidate' else 'windows',
                                   'Tags':[{'Key':'cloud-provisioning-test','Value':'cldt-test' if owned else 'other'}]}]}
            elif operation=='get-role': result={'Role':{'Arn':'arn:role','Tags':[{'Key':'cloud-provisioning-test','Value':'cldt-test'}]}}
            elif operation=='put-role-policy': mutations.append(inputs);result={}
            else:self.fail(operation)
            return subprocess.CompletedProcess(args,0,json.dumps(result),'')
        error=None
        argv=[str(SCRIPT),'--work-dir',str(root),'--awsnode','/unused','--phase','authorize']
        if gpu:argv+=['--variant','gpu','--windows-version','2025']
        with patch.object(sys,'argv',argv),patch('subprocess.run',run),patch('subprocess.check_output',return_value=json.dumps(policy)):
            try:runpy.run_path(str(SCRIPT),run_name='__main__')
            except SystemExit as e:self.assertEqual(e.code,0)
            except RuntimeError as e:error=e
        return root,mutations,error

    def test_windows_authorization_retains_additional_linux_candidates(self):
        _,mutations,error=self.authorize(extra_linux=True)
        self.assertIsNone(error)
        resources=json.loads(mutations[0]['PolicyDocument'])['Statement'][0]['Resource']
        self.assertIn('arn:aws:ec2:us-west-2::image/ami-linux-candidate',resources)

    def test_exact_windows_images_added_and_linux_retained(self):
        root,mutations,error=self.authorize()
        self.assertIsNone(error)
        resources=json.loads(mutations[0]['PolicyDocument'])['Statement'][0]['Resource']
        self.assertEqual(set(resources),{'arn:aws:ec2:us-west-2::image/ami-linux','arn:subnet',
                                        'arn:aws:ec2:us-west-2::image/ami-2022','arn:aws:ec2:us-west-2::image/ami-2025'})
        self.assertEqual(len(json.loads((root/'resources.json').read_text())['authorizedWindowsAMIs']),2)

    def test_pending_images_cannot_change_role(self):
        root,mutations,error=self.authorize(image_state='pending')
        self.assertIsNotNone(error);self.assertEqual(mutations,[])

    def test_foreign_images_cannot_change_role(self):
        root,mutations,error=self.authorize(owned=False)
        self.assertIsNotNone(error);self.assertEqual(mutations,[])

    def test_gpu_authorization_preserves_cpu_and_linux_permissions(self):
        root,mutations,error=self.authorize(gpu=True)
        self.assertIsNone(error)
        resources=json.loads(mutations[0]['PolicyDocument'])['Statement'][0]['Resource']
        self.assertEqual(set(resources),{'arn:aws:ec2:us-west-2::image/'+image
                                        for image in ['ami-linux','ami-2022','ami-2025','ami-gpu-2025']} | {'arn:subnet'})
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['windowsGpuImageBuilds']['2025']['imageState'],'available')
        self.assertEqual(set(state['authorizedWindowsAMIs']),{'ami-2022','ami-2025','ami-gpu-2025'})

    def test_pending_gpu_candidate_does_not_change_role(self):
        root,mutations,error=self.authorize(gpu=True,image_state='pending')
        self.assertIsNotNone(error)
        self.assertEqual(mutations,[])


class WindowsImageRevision(unittest.TestCase):
    def revise(self, owned=True, worker_only=False, gpu_cache=False, supersede=None):
        import hashlib
        import runpy
        import sys
        from unittest.mock import patch
        temp=tempfile.TemporaryDirectory();self.addCleanup(temp.cleanup)
        root=pathlib.Path(temp.name)
        (root/'service.exe').write_bytes(b'changed image artifact')
        state={'account':'123','region':'us-west-2','runID':'cldt-test','vpcID':'vpc-owned',
               'windowsProbeInstances':{y:{'instanceID':'i-'+y} for y in ['2022','2025']},
               'windowsImageBuilds':{y:{'imageID':'ami-'+y,'tunnelSHA256':'a'*64,'generalizeRequestedAt':'before'} for y in ['2022','2025']}}
        if worker_only:
            (root/'worker.exe').write_bytes(b'new worker artifact')
            for record in state['windowsImageBuilds'].values():
                record.update(tunnelSHA256=hashlib.sha256((root/'service.exe').read_bytes()).hexdigest(),workerSHA256='b'*64,workerVersion='v1.36.2+k0s.0')
        if gpu_cache:
            from images.windows.cache import make_recipe
            fixtures=SCRIPT.parent.parent/'observe/testdata'
            runtime=json.loads((fixtures/'windows-cache-runtime.json').read_text())
            snapshot=json.loads((fixtures/'windows-cache-ready.json').read_text())
            recipe=make_recipe(runtime,snapshot['registeredImages'],'2022')
            (root/'cache.json').write_text(json.dumps(recipe))
            state['windowsGpuBuilders']={y:{'instanceID':'i-'+y,'baseImageID':'ami-'+y} for y in ['2022','2025']}
            state['windowsGpuImageBuilds']={}
            for year,base in state['windowsImageBuilds'].items():
                base.update(tunnelSHA256=hashlib.sha256((root/'service.exe').read_bytes()).hexdigest(),workerSHA256=runtime['workerSHA256'])
                state['windowsGpuImageBuilds'][year]=dict(base,imageID='ami-gpu-'+year,gpu={'packageSHA256':'d'*64})
            if supersede:
                old=make_recipe(runtime,recipe['images']+['example.invalid/rejected@sha256:'+'f'*64],'2022')
                state['windowsGpuImageBuilds']['2022']=dict(revisionOf='ami-gpu-2022',
                    workerSHA256=runtime['workerSHA256'],revisionRequestedAt='original-request',
                    requestedSHA256=hashlib.sha256((root/'service.exe').read_bytes()).hexdigest(),
                    requestedWorker={},requestedCacheRecipe=old)
                if supersede=='prepared':state['windowsGpuImageBuilds']['2022']['preparedAt']='already-prepared'
                if supersede=='service':state['windowsGpuImageBuilds']['2022']['requestedSHA256']='e'*64
        (root/'resources.json').write_text(json.dumps(state))
        mutations=[]
        def run(args,**kw):
            operation=args[4];inputs=json.loads(args[args.index('--cli-input-json')+1])
            if operation=='get-caller-identity':result={'Account':'123'}
            elif operation=='describe-instances':result={'Reservations':[{'Instances':[{'InstanceId':'i-'+y,'VpcId':'vpc-owned','Platform':'windows','NetworkInterfaces':[{}],'State':{'Name':'running' if supersede=='running' else 'stopped'},'Tags':[{'Key':'cloud-provisioning-test','Value':'cldt-test'}]} for y in ['2022','2025']]}]}
            elif operation=='describe-images':result={'Images':[{'State':'available','Public':False,'OwnerId':'123','Tags':[{'Key':'cloud-provisioning-test','Value':'cldt-test' if owned else 'other'}]}]}
            elif operation in ['modify-instance-attribute','start-instances']:
                mutations.append((operation,inputs));result={}
            else:self.fail(operation)
            return subprocess.CompletedProcess(args,0,json.dumps(result),'')
        error=None
        argv=[str(SCRIPT),'--work-dir',str(root),'--awsnode','/unused','--phase','revise','--service',str(root/'service.exe')]
        if worker_only:
            argv+=['--worker',str(root/'worker.exe'),'--worker-sha256',hashlib.sha256((root/'worker.exe').read_bytes()).hexdigest(),'--worker-version','v1.36.2+k0s.0.appmana.1']
        if gpu_cache:
            argv+=['--variant','gpu','--windows-version','2022','--cache-recipe',str(root/'cache.json')]
        if supersede:
            argv+=['--supersede-cache-sha256','0'*64 if supersede=='stale' else old['sha256']]
        with patch.object(sys,'argv',argv),patch('subprocess.run',run):
            try:runpy.run_path(str(SCRIPT),run_name='__main__')
            except RuntimeError as e:error=e
        return root,mutations,error

    def test_revision_retains_capture_history_and_clears_old_userdata(self):
        root,mutations,error=self.revise()
        self.assertIsNone(error)
        state=json.loads((root/'resources.json').read_text())
        for year in ['2022','2025']:
            self.assertEqual(state['windowsImageBuildHistory'][year][0]['imageID'],'ami-'+year)
            self.assertEqual(state['windowsImageBuilds'][year]['revisionOf'],'ami-'+year)
            self.assertNotIn('generalizeRequestedAt',state['windowsImageBuilds'][year])
        self.assertEqual([op for op,inputs in mutations],['modify-instance-attribute','start-instances']*2)
        for op,inputs in mutations:
            if op=='modify-instance-attribute':self.assertEqual(inputs['UserData'],{'Value':''})

    def test_revision_requires_owned_previous_capture(self):
        root,mutations,error=self.revise(owned=False)
        self.assertIsNotNone(error);self.assertEqual(mutations,[])

    def test_worker_only_revision_preserves_previous_image_and_requested_identity(self):
        root,mutations,error=self.revise(worker_only=True)
        self.assertIsNone(error)
        state=json.loads((root/'resources.json').read_text())
        for year in ['2022','2025']:
            self.assertEqual(state['windowsImageBuilds'][year]['requestedWorker']['workerVersion'],'v1.36.2+k0s.0.appmana.1')
            self.assertEqual(state['windowsImageBuildHistory'][year][0]['workerSHA256'],'b'*64)
        self.assertEqual(len(mutations),4)

    def test_gpu_cache_revision_preserves_cpu_and_other_os_history(self):
        root,mutations,error=self.revise(gpu_cache=True)
        self.assertIsNone(error)
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['windowsImageBuilds']['2022']['imageID'],'ami-2022')
        self.assertEqual(state['windowsGpuImageBuilds']['2025']['imageID'],'ami-gpu-2025')
        self.assertEqual(state['windowsGpuImageBuildHistory']['2022'][0]['imageID'],'ami-gpu-2022')
        self.assertEqual(state['windowsGpuImageBuildHistory']['2022'][0]['gpu']['packageSHA256'],'d'*64)
        revised=state['windowsGpuImageBuilds']['2022']
        self.assertEqual(revised['revisionOf'],'ami-gpu-2022')
        self.assertNotIn('generalizeRequestedAt',revised)
        self.assertTrue(revised['requestedCacheRecipe']['images'])
        self.assertEqual([op for op,_ in mutations],['modify-instance-attribute','start-instances'])

    def test_explicit_cache_supersession_preserves_rejected_intent(self):
        root,mutations,error=self.revise(gpu_cache=True,supersede='valid')
        self.assertIsNone(error)
        state=json.loads((root/'resources.json').read_text())
        record=state['windowsGpuImageBuilds']['2022']
        old=record['supersededCacheRevisions'][0]
        self.assertEqual(old['revisionRequestedAt'],'original-request')
        self.assertNotEqual(old['requestedCacheRecipe']['sha256'],record['requestedCacheRecipe']['sha256'])
        self.assertEqual(record['revisionOf'],'ami-gpu-2022')
        self.assertNotIn('preparedAt',record)
        self.assertEqual(state['windowsImageBuilds']['2022']['imageID'],'ami-2022')
        self.assertEqual(state['windowsGpuImageBuilds']['2025']['imageID'],'ami-gpu-2025')
        self.assertEqual([op for op,_ in mutations],['modify-instance-attribute','start-instances'])

    def test_cache_supersession_rejects_active_prepared_or_changed_intents(self):
        for mode in ['running','prepared','stale','service']:
            with self.subTest(mode=mode):
                root,mutations,error=self.revise(gpu_cache=True,supersede=mode)
                self.assertIsNotNone(error)
                self.assertEqual(mutations,[])
                record=json.loads((root/'resources.json').read_text())['windowsGpuImageBuilds']['2022']
                self.assertNotIn('supersededCacheRevisions',record)


class WindowsImageRootVolumeStatus(unittest.TestCase):
    def test_status_records_exact_root_mapping_and_rejects_invalid_metadata(self):
        import runpy
        from unittest.mock import patch
        for size in [100, None, True, '100', 0]:
            with self.subTest(size=size), tempfile.TemporaryDirectory() as directory:
                root=pathlib.Path(directory)
                state=dict(account='123',region='us-west-2',runID='owned-run',vpcID='vpc-owned',
                           windowsProbeInstances={'2025':{'instanceID':'i-2025'}},
                           windowsImageBuilds={'2025':{'imageID':'ami-2025','imageState':'pending'}},
                           retiredImages=[{'id':'ami-2025','snapshots':[]}])
                path=root/'resources.json';path.write_text(json.dumps(state))
                def call(args,**kwargs):
                    operation=args[4]
                    if operation=='get-caller-identity':result={'Account':'123'}
                    elif operation=='describe-instances':
                        result={'Reservations':[{'Instances':[dict(InstanceId='i-2025',VpcId='vpc-owned',Platform='windows',NetworkInterfaces=[{}],State={'Name':'stopped'},Tags=[{'Key':'cloud-provisioning-test','Value':'owned-run'}])]}]}
                    elif operation=='describe-images':
                        result={'Images':[dict(Public=False,OwnerId='123',State='available',RootDeviceName='/dev/sda1',BlockDeviceMappings=[{'DeviceName':'/dev/sdb','Ebs':{'VolumeSize':500,'SnapshotId':'snap-data'}},{'DeviceName':'/dev/sda1','Ebs':{'VolumeSize':size,'SnapshotId':'snap-root'}}])]}
                    else:self.fail(operation)
                    return subprocess.CompletedProcess(args,0,json.dumps(result),'')
                args=[str(SCRIPT),'--work-dir',str(root),'--awsnode','/unused','--phase','status','--windows-version','2025']
                with patch.object(sys,'argv',args),patch('subprocess.run',call):
                    if type(size) is int and size>0:
                        runpy.run_path(str(SCRIPT),run_name='__main__')
                        recorded=json.loads(path.read_text())['windowsImageBuilds']['2025']
                        self.assertEqual(recorded['rootVolumeGiB'],100)
                        self.assertEqual(recorded['imageState'],'available')
                    else:
                        with self.assertRaisesRegex(RuntimeError,'root volume'):
                            runpy.run_path(str(SCRIPT),run_name='__main__')
                        self.assertEqual(json.loads(path.read_text()),state)

if __name__=='__main__':unittest.main()
