import base64
import hashlib
import json
import pathlib
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = pathlib.Path(__file__).with_name('publisher.py')

class PublisherTest(unittest.TestCase):
    def exercise(self, owner='cldt-test', observed_digest='sha256:abc', component='windows-publisher', archive=False, renew=False, recorded=None, missing_repo=False, windows_version=None, image_build='20348', forged_descriptor=False):
        temp = tempfile.TemporaryDirectory(); self.addCleanup(temp.cleanup)
        root = pathlib.Path(temp.name)
        (root/'resources.json').write_text(json.dumps(dict(runID='cldt-test', account='123', region='us-west-2', publisherImage=(recorded or '123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/windows-publisher@sha256:abc') if renew else 'original-publisher')))
        (root/'index.json').write_text(json.dumps({'manifests': [{'digest': 'sha256:abc'}]}))
        if windows_version:
            state=json.loads((root/'resources.json').read_text())
            state['windowsGPUProbeImage']='legacy-2025-image'
            state['windowsGPUProbeByWindowsVersion']={'2025':{'image':'recorded-2025-image'}}
            (root/'resources.json').write_text(json.dumps(state))
            blobs=root/'blobs'/'sha256';blobs.mkdir(parents=True)
            def blob(obj):
                raw=json.dumps(obj).encode();digest=hashlib.sha256(raw).hexdigest();(blobs/digest).write_bytes(raw);return 'sha256:'+digest
            platform={'os':'windows','architecture':'amd64','os.version':'10.0.'+image_build+'.5499'}
            manifest=blob({'config':{'digest':blob(platform)}})
            descriptor=dict(platform)
            if forged_descriptor:descriptor['os.version']='10.0.20348.5499'
            (root/'index.json').write_text(json.dumps({'manifests':[{'digest':manifest,'platform':descriptor}]}))
            observed_digest=manifest
            if renew:
                state['windowsGPUProbeByWindowsVersion'][windows_version]={'image':'123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/'+component+'@'+manifest,'platform':platform}
                (root/'resources.json').write_text(json.dumps(state))
        repo = dict(repositoryArn='arn:aws:ecr:us-west-2:123:repository/cldt-test/'+component, repositoryUri='123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/'+component)
        calls=[]; published=[]
        def run(args, **kw):
            calls.append((args, kw))
            result={}
            if args[0]=='aws':
                operation=args[4]
                if operation=='get-caller-identity': result={'Account':'123'}
                elif operation=='describe-repositories':
                    if missing_repo:return subprocess.CompletedProcess(args,1,'','RepositoryNotFoundException')
                    result={'repositories':[repo]}
                elif operation=='list-tags-for-resource': result={'tags':[{'Key':'cloud-provisioning-test','Value':owner}]}
                elif operation=='get-federation-token': result={'Credentials':dict(AccessKeyId='PULL',SecretAccessKey='secret',SessionToken='session',Expiration='later')}
                elif operation=='get-authorization-token':
                    restricted=(kw.get('env') or {}).get('AWS_ACCESS_KEY_ID')=='PULL'
                    result={'authorizationData':[{'authorizationToken':'read-only' if restricted else 'setup-push'}]}
                else: self.fail(operation)
            elif args[0]=='crane':
                if args[1]=='digest': return subprocess.CompletedProcess(args,0,'sha256:abc' if '--tarball' in args else observed_digest,'')
            elif args[0]=='docker': published.append(json.loads(kw['input']))
            else: self.fail(args)
            return subprocess.CompletedProcess(args,0,json.dumps(result),'')
        argv=[str(SCRIPT),'--work-dir',str(root)]+(['--renew-pull-only'] if renew else ['--archive' if archive else '--layout',str(root)])+['--component',component,'--api-server','https://10.10.0.10:6443']
        if windows_version:argv+=['--windows-version',windows_version]
        error=None
        with patch.object(sys,'argv',argv), patch('subprocess.run',run):
            try: runpy.run_path(str(SCRIPT),run_name='__main__')
            except RuntimeError as e: error=e
        return root, calls, published, error

    def test_cluster_receives_only_repository_scoped_read_token(self):
        root,calls,published,error=self.exercise()
        self.assertIsNone(error)
        self.assertEqual(len(published),1)
        config=json.loads(base64.b64decode(published[0]['data']['.dockerconfigjson']))
        self.assertEqual(next(iter(config['auths'].values()))['auth'],'read-only')
        args=next(args for args,kw in calls if args[0]=='aws' and args[4]=='get-federation-token')
        inputs=json.loads(args[args.index('--cli-input-json')+1])
        policy=json.loads(inputs['Policy'])
        repo_statement=policy['Statement'][1]
        self.assertEqual(set(repo_statement['Action']),{'ecr:BatchGetImage','ecr:GetDownloadUrlForLayer','ecr:BatchCheckLayerAvailability'})
        self.assertTrue(repo_statement['Resource'].endswith('/cldt-test/windows-publisher'))
        state=json.loads((root/'resources.json').read_text())
        self.assertIn('publisherRepository',state)
        self.assertTrue(state['publisherImage'].endswith('@sha256:abc'))

    def test_versioned_workload_publication_preserves_other_windows_images(self):
        root,calls,published,error=self.exercise(component='windows-gpu-probe',windows_version='2022')
        self.assertIsNone(error)
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['windowsGPUProbeImage'],'legacy-2025-image')
        versions=state['windowsGPUProbeByWindowsVersion']
        self.assertEqual(versions['2025']['image'],'recorded-2025-image')
        self.assertEqual(versions['2022']['platform']['os.version'],'10.0.20348.5499')
        self.assertIn('/windows-gpu-probe@sha256:',versions['2022']['image'])
        self.assertEqual(state['windowsGPUProbeRepository'],versions['2022']['repository'])
        self.assertEqual(state['windowsGPUProbePullSecret'],versions['2022']['pullSecret'])
        self.assertEqual(len(published),1)

    def test_wrong_workload_build_is_rejected_before_cloud_access(self):
        for forged in [False,True]:
            with self.subTest(forged_descriptor=forged):
                root,calls,published,error=self.exercise(component='windows-gpu-probe',windows_version='2022',image_build='26100',forged_descriptor=forged)
                self.assertIsNotNone(error)
                self.assertEqual(calls,[])
                self.assertEqual(published,[])

    def test_versioned_pull_renewal_uses_original_digest_without_push(self):
        root,calls,published,error=self.exercise(component='windows-gpu-probe',windows_version='2022',renew=True)
        self.assertIsNone(error)
        self.assertFalse(any(args[:2]==['crane','push'] for args,_ in calls))
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['windowsGPUProbeImage'],'legacy-2025-image')
        record=state['windowsGPUProbeByWindowsVersion']['2022']
        self.assertEqual(record['pullSessionExpiresAt'],'later')
        self.assertEqual(record['platform']['os.version'],'10.0.20348.5499')
        config=json.loads(base64.b64decode(published[0]['data']['.dockerconfigjson']))
        self.assertEqual(next(iter(config['auths'].values()))['auth'],'read-only')

    def test_foreign_repository_not_pushed(self):
        root,calls,published,error=self.exercise(owner='other')
        self.assertIsNotNone(error)
        self.assertFalse(any(args[0]=='crane' for args,kw in calls))
        self.assertEqual(published,[])

    def test_digest_mismatch_not_published_to_cluster(self):
        root,calls,published,error=self.exercise(observed_digest='sha256:different')
        self.assertIsNotNone(error)
        self.assertEqual(published,[])
        self.assertIn('publisherRepository',json.loads((root/'resources.json').read_text()))

    def test_calico_archive_uses_separate_repository_namespace_and_state(self):
        root,calls,published,error=self.exercise(component='calico-node-windows', archive=True)
        self.assertIsNone(error)
        self.assertEqual(published[0]['metadata']['namespace'],'kube-system')
        self.assertEqual(published[0]['metadata']['name'],'cldt-test-calico-pull')
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['publisherImage'],'original-publisher')
        self.assertTrue(state['calicoWindowsImage'].endswith('/calico-node-windows@sha256:abc'))
        request=next(args for args,kw in calls if args[0]=='aws' and args[4]=='get-federation-token')
        policy=json.loads(json.loads(request[request.index('--cli-input-json')+1])['Policy'])
        self.assertTrue(policy['Statement'][1]['Resource'].endswith('/cldt-test/calico-node-windows'))
        config=json.loads(base64.b64decode(published[0]['data']['.dockerconfigjson']))
        self.assertEqual(next(iter(config['auths'].values()))['auth'],'read-only')
        self.assertTrue(any(args[:2]==['crane','digest'] and '--tarball' in args for args,kw in calls))

    def test_gpu_plugin_gets_only_its_repository_pull_access(self):
        root,calls,published,error=self.exercise(component='windows-gpu-device-plugin', archive=True)
        self.assertIsNone(error)
        self.assertEqual(published[0]['metadata']['namespace'],'cldt-windows-gpu')
        self.assertEqual(published[0]['metadata']['name'],'cldt-test-gpu-plugin-pull')
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['publisherImage'],'original-publisher')
        self.assertTrue(state['windowsGPUPluginImage'].endswith('/windows-gpu-device-plugin@sha256:abc'))
        request=next(args for args,kw in calls if args[0]=='aws' and args[4]=='get-federation-token')
        policy=json.loads(json.loads(request[request.index('--cli-input-json')+1])['Policy'])
        self.assertTrue(policy['Statement'][1]['Resource'].endswith('/cldt-test/windows-gpu-device-plugin'))
        self.assertNotIn('ecr:PutImage',policy['Statement'][1]['Action'])

    def test_gpu_probe_gets_only_its_repository_pull_access(self):
        root,calls,published,error=self.exercise(component='windows-gpu-probe', archive=False)
        self.assertIsNone(error)
        self.assertEqual(published[0]['metadata']['namespace'],'cldt-windows-gpu')
        self.assertEqual(published[0]['metadata']['name'],'cldt-test-gpu-probe-pull')
        state=json.loads((root/'resources.json').read_text())
        self.assertEqual(state['publisherImage'],'original-publisher')
        self.assertTrue(state['windowsGPUProbeImage'].endswith('/windows-gpu-probe@sha256:abc'))
        request=next(args for args,kw in calls if args[0]=='aws' and args[4]=='get-federation-token')
        policy=json.loads(json.loads(request[request.index('--cli-input-json')+1])['Policy'])
        self.assertTrue(policy['Statement'][1]['Resource'].endswith('/cldt-test/windows-gpu-probe'))
        self.assertNotIn('ecr:PutImage',policy['Statement'][1]['Action'])

    def test_calico_archive_digest_mismatch_does_not_publish_secret(self):
        root,calls,published,error=self.exercise(component='calico-node-windows', archive=True, observed_digest='sha256:different')
        self.assertIsNotNone(error)
        self.assertEqual(published,[])
        state=json.loads((root/'resources.json').read_text())
        self.assertIn('calicoWindowsRepository',state)
        self.assertNotIn('calicoWindowsImage',state)
        self.assertEqual(state['publisherImage'],'original-publisher')


    def test_pull_renewal_never_pushes_or_uses_setup_registry_token(self):
        root,calls,published,error=self.exercise(renew=True)
        self.assertIsNone(error)
        self.assertEqual(len(published),1)
        self.assertFalse(any(args[:2]==['crane','push'] for args,kw in calls))
        auth=[kw for args,kw in calls if args[0]=='aws' and args[4]=='get-authorization-token']
        self.assertEqual(len(auth),1)
        self.assertEqual(auth[0]['env']['AWS_ACCESS_KEY_ID'],'PULL')
        self.assertEqual(json.loads((root/'resources.json').read_text())['publisherImage'],'123.dkr.ecr.us-west-2.amazonaws.com/cldt-test/windows-publisher@sha256:abc')

    def test_renewal_rejects_foreign_image_and_failed_readback(self):
        for kwargs in [dict(recorded='foreign.example/image@sha256:abc'),dict(observed_digest='sha256:wrong')]:
            root,calls,published,error=self.exercise(renew=True,**kwargs)
            self.assertIsNotNone(error)
            self.assertEqual(published,[])
            self.assertFalse(any(args[:2]==['crane','push'] for args,kw in calls))

    def test_renewal_cannot_create_repository_or_invent_an_image(self):
        for kwargs in [dict(missing_repo=True),dict(recorded='not-an-immutable-image')]:
            root,calls,published,error=self.exercise(renew=True,**kwargs)
            self.assertIsNotNone(error)
            self.assertEqual(published,[])
            self.assertFalse(any(args[0]=='aws' and args[4]=='create-repository' for args,kw in calls))
            self.assertFalse(any(args[:2]==['crane','push'] for args,kw in calls))

if __name__=='__main__': unittest.main()
