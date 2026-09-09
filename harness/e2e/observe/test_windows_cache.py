import copy
import json
import pathlib
import unittest
from windows_cache import evaluate
from images.windows.cache import make_recipe, validate_recipe, validate_extension, verify_preparation


class WindowsCache(unittest.TestCase):
    def test_native_windows2022_complete_cache_preserves_all_alias_targets(self):
        fixture = json.loads((pathlib.Path(__file__).with_name('testdata') /
                              'windows-cache-complete-ready-2022.json').read_text())
        recipe = fixture['recipe']
        self.assertEqual(fixture['status'], 'prepared')
        self.assertIs(fixture['registryCredentialsRemoved'], True)
        self.assertEqual(len(fixture['snapshot']['readyImages']), 16)
        self.assertTrue(verify_preparation(fixture, recipe, '2022')['passed'])
        # Readiness alone is insufficient if a mutable startup alias points
        # at content other than the pinned image observed on the builder.
        changed = copy.deepcopy(fixture)
        alias = recipe['aliases'][0]['reference']
        changed['snapshot']['imageTargets'][alias] = 'sha256:' + '0' * 64
        with self.assertRaises(ValueError):
            verify_preparation(changed, recipe, '2022')
        with self.assertRaises(ValueError):
            verify_preparation(fixture, recipe, '2025')

    def setUp(self):
        root = pathlib.Path(__file__).with_name('testdata')
        self.snapshot = json.loads((root/'windows-cache-unpacking.json').read_text())
        self.runtime = json.loads((root/'windows-cache-runtime.json').read_text())
        self.ready = json.loads((root/'windows-cache-ready.json').read_text())
        self.images = self.snapshot['registeredImages']

    def check(self, snapshot):
        return evaluate(snapshot, self.runtime, self.images, '2022')

    def test_native_successful_pulls_do_not_prove_host_platform_unpacked(self):
        fixture=json.loads((pathlib.Path(__file__).with_name('testdata')/
                            'windows-cache-platform-failure.json').read_text())
        self.assertTrue(all(pull['exitCode']==0 for pull in fixture['pulls']))
        recipe=fixture['recipe']
        result=evaluate(fixture['snapshot'],recipe['runtime'],recipe['images'],
                        '2025',recipe['aliases'])
        self.assertEqual([key for key,passed in result['checks'].items() if not passed],
                         ['allImagesDownloadedAndUnpacked'])
        self.assertEqual(len(fixture['snapshot']['registeredImages']),14)
        self.assertEqual(len(fixture['snapshot']['readyImages']),10)
        with self.assertRaises(ValueError):verify_preparation(fixture,recipe,'2025')

    def test_native_default_matcher_recovery_passes_all_cache_gates(self):
        fixture=json.loads((pathlib.Path(__file__).with_name('testdata')/
                            'windows-cache-native-unpack-ready.json').read_text())
        self.assertEqual(fixture['status'],'prepared')
        self.assertEqual(len(fixture['snapshot']['readyImages']),14)
        self.assertTrue(verify_preparation(fixture,fixture['recipe'],'2025')['passed'])
        changed=copy.deepcopy(fixture)
        changed['snapshot']['readyImages'].pop()
        with self.assertRaises(ValueError):verify_preparation(changed,fixture['recipe'],'2025')

    def test_native_registered_image_is_not_ready_during_unpack(self):
        result = self.check(self.snapshot)
        self.assertFalse(result['passed'])
        self.assertEqual([k for k, v in result['checks'].items() if not v],
                         ['allImagesDownloadedAndUnpacked'])

    def test_native_two_image_preparation_completed_and_removed_credentials(self):
        receipt=json.loads((pathlib.Path(__file__).with_name('testdata')/
                            'windows-cache-two-images-prepared-2025.json').read_text())
        self.assertEqual(receipt['status'],'prepared')
        self.assertEqual(receipt['exitCode'],0)
        self.assertIs(receipt['registryCredentialsRemoved'],True)
        recipe=make_recipe(self.runtime,receipt['snapshot']['registeredImages'],'2025')
        self.assertTrue(verify_preparation(receipt,recipe,'2025')['passed'])

    def test_native_partial_multi_image_cache_is_not_capture_ready(self):
        snapshot=json.loads((pathlib.Path(__file__).with_name('testdata')/
                             'windows-cache-two-images-unpacking-2025.json').read_text())
        self.assertEqual(len(snapshot['registeredImages']),2)
        self.assertEqual(len(snapshot['readyImages']),1)
        result=evaluate(snapshot,self.runtime,snapshot['registeredImages'],'2025')
        self.assertFalse(result['passed'])
        self.assertEqual([key for key,passed in result['checks'].items() if not passed],
                         ['allImagesDownloadedAndUnpacked'])
        recipe=make_recipe(self.runtime,snapshot['registeredImages'],'2025')
        with self.assertRaises(ValueError):
            verify_preparation(dict(snapshot=snapshot,serviceStopped=True,
                                    cacheServiceRemoved=True),recipe,'2025')

    def test_native_completion_only_passes_builder_readiness(self):
        result = self.check(self.ready)
        self.assertTrue(result['passed'])
        self.assertIn('fresh-clone reuse remain separate gates', result['scope'])

    def test_identity_runtime_and_workload_drift_fail(self):
        ready = self.ready
        for change in [{'workerServicePresent': True}, {'identityPathsPresent': ['pki']},
                       {'containers': ['workload']}, {'tasks': ['task']},
                       {'workerSHA256': '0'*64}, {'containerdSHA256': '0'*64},
                       {'shimSHA256': '0'*64}, {'windowsBuild': 26100},
                       {'root': r'C:\temporary'}, {'namespace': 'default'},
                       {'cacheService': 'Stopped'}, {'registeredImages': []}]:
            with self.subTest(change=change):
                self.assertFalse(self.check(dict(ready, **change))['passed'])

    def test_missing_empty_lists_are_not_cleanliness_evidence(self):
        for key in ['containers', 'tasks', 'identityPathsPresent']:
            snapshot = dict(self.ready)
            del snapshot[key]
            self.assertFalse(self.check(snapshot)['passed'])

    def test_ambiguous_runtime_and_unpinned_images_are_rejected(self):
        for images in [[], self.images*2, ['example:latest']]:
            with self.assertRaises(ValueError):
                evaluate(self.snapshot, self.runtime, images, '2022')
        runtime = copy.deepcopy(self.runtime)
        runtime['components'].append(runtime['components'][0])
        with self.assertRaises(ValueError):
            evaluate(self.snapshot, runtime, self.images, '2022')

    def test_recipe_changes_with_os_or_image_and_rejects_digest_drift(self):
        recipe=make_recipe(self.runtime,self.images,'2022')
        self.assertEqual(validate_recipe(recipe,'2022'),recipe)
        self.assertEqual(recipe['dataDirectoryPermissions'],'inherit-parent')
        with self.assertRaises(ValueError):
            validate_recipe(dict(recipe,schemaVersion=1),'2022')
        self.assertNotEqual(recipe['sha256'],make_recipe(self.runtime,self.images,'2025')['sha256'])
        with self.assertRaises(ValueError):
            validate_recipe(dict(recipe,sha256='0'*64),'2022')
        with self.assertRaises(ValueError):
            validate_recipe(recipe,'2025')

    def test_capture_preparation_requires_cache_ready_and_service_removed(self):
        recipe=make_recipe(self.runtime,self.images,'2022')
        # Simulate a corrected builder; historical observations predate the ACL gate.
        receipt={'snapshot':dict(self.ready,dataDirectoryInheritsPermissions=True),'serviceStopped':True,'cacheServiceRemoved':True}
        self.assertTrue(verify_preparation(receipt,recipe,'2022')['passed'])
        for change in [{'snapshot':self.snapshot},{'snapshot':self.ready},
                       {'snapshot':dict(self.ready,dataDirectoryInheritsPermissions=False)},
                       {'serviceStopped':False},{'cacheServiceRemoved':False}]:
            with self.subTest(change=change),self.assertRaises(ValueError):
                verify_preparation(dict(receipt,**change),recipe,'2022')

    def test_native_windows2025_preparation_exits_successfully_and_removes_service(self):
        receipt=json.loads((pathlib.Path(__file__).with_name('testdata')/'windows-cache-preparation-2025.json').read_text())
        recipe=make_recipe(self.runtime,self.images,'2025')
        # The observed v1 image layout predates the inherited-permissions fix.
        self.assertNotEqual(receipt['recipeSHA256'],recipe['sha256'])
        self.assertEqual(receipt['status'],'prepared')
        self.assertEqual([p['exitCode'] for p in receipt['pulls']],[0])
        self.assertTrue(evaluate(receipt['snapshot'],self.runtime,self.images,'2025')['passed'])
        # Successful unpack/shutdown did not establish the permissions needed
        # by Local Service when the clone starts its Traefik static Pod.
        with self.assertRaisesRegex(ValueError,'inherited data-directory permissions'):
            verify_preparation(receipt,recipe,'2025')

    def test_corrected_native_builder_passes_permission_and_shutdown_gates(self):
        for year in ['2022','2025']:
            with self.subTest(year=year):
                receipt=json.loads((pathlib.Path(__file__).with_name('testdata')/f'windows-cache-preparation-v2-{year}.json').read_text())
                recipe=make_recipe(self.runtime,self.images,year)
                self.assertTrue(verify_preparation(receipt,recipe,year)['passed'])
                for value in [None,False]:
                    changed=copy.deepcopy(receipt)
                    changed['snapshot']['dataDirectoryInheritsPermissions']=value
                    with self.assertRaisesRegex(ValueError,'inherited data-directory permissions'):
                        verify_preparation(changed,recipe,year)


class AliasRecipeTest(unittest.TestCase):
    def setUp(self):
        root=pathlib.Path(__file__).with_name('testdata')
        self.native=json.loads((root/'windows-cache-alias-native.json').read_text())['versions']
        self.runtime={'schemaVersion':1,'workerSHA256':'a'*64,'components':[{'name':'containerd.exe','sha256':'b'*64},{'name':'containerd-shim-runhcs-v1.exe','sha256':'c'*64}]}

    def test_extension_preserves_existing_references_and_runtime(self):
        source=self.native['2025']['source']
        alias={'reference':self.native['2025']['alias'],'image':source}
        old=make_recipe(self.runtime,[source],'2025')
        extended=make_recipe(self.runtime,[source],'2025',[alias])
        self.assertEqual(validate_extension(old,extended,'2025'),extended)
        with self.assertRaises(ValueError):validate_extension(old,old,'2025')
        with self.assertRaises(ValueError):validate_extension(extended,old,'2025')
        changed=copy.deepcopy(self.runtime);changed['components'][0]['sha256']='0'*64
        with self.assertRaises(ValueError):
            validate_extension(old,make_recipe(changed,[source],'2025',[alias]),'2025')
        other=source.split('@')[0]+'@sha256:'+'0'*64
        retargeted=make_recipe(self.runtime,[source,other],'2025',[dict(alias,image=other)])
        with self.assertRaises(ValueError):validate_extension(extended,retargeted,'2025')
        with self.assertRaises(ValueError):
            validate_extension(old,make_recipe(self.runtime,[other],'2025'),'2025')

    def test_native_aliases_bind_to_same_pinned_content_on_both_builds(self):
        for year,native in self.native.items():
            alias={'reference':native['alias'],'image':native['source']}
            recipe=make_recipe(self.runtime,[native['source']],year,[alias])
            self.assertEqual(recipe['schemaVersion'],3)
            self.assertEqual(validate_recipe(recipe,year),recipe)
            self.assertEqual(native['sourceTarget'],native['aliasTarget'])
            # Selected native descriptor identities feed a simulated builder
            # snapshot; native alias cleanup is not fresh-clone cache proof.
            snapshot={'registeredImages':[native['source'],native['alias']],
                      'readyImages':[native['source'],native['alias']],
                      'imageTargets':{native['source']:native['sourceTarget'],native['alias']:native['aliasTarget']}}
            result=evaluate(snapshot,self.runtime,[native['source']],year,[alias])
            self.assertTrue(result['checks']['exactImagesRegistered'])
            self.assertTrue(result['checks']['exactImageTargets'])
            self.assertFalse(result['passed'])  # no worker/runtime/cleanliness proof
            snapshot['imageTargets'][native['alias']]='sha256:'+'0'*64
            self.assertFalse(evaluate(snapshot,self.runtime,[native['source']],year,[alias])['checks']['exactImageTargets'])
            del snapshot['imageTargets']
            self.assertFalse(evaluate(snapshot,self.runtime,[native['source']],year,[alias])['checks']['exactImageTargets'])

    def test_alias_recipe_cannot_hide_missing_alias_or_retargeting(self):
        native=self.native['2022'];alias={'reference':native['alias'],'image':native['source']}
        recipe=make_recipe(self.runtime,[native['source']],'2022',[alias])
        changed=copy.deepcopy(recipe);changed['aliases'][0]['reference']=native['alias']+'-changed'
        with self.assertRaises(ValueError):validate_recipe(changed,'2022')
        with self.assertRaises(ValueError):validate_recipe(dict(recipe,aliases=[]),'2022')
        result=evaluate({'registeredImages':[native['source']],'readyImages':[native['source']]},self.runtime,[native['source']],'2022',[alias])
        self.assertFalse(result['checks']['exactImagesRegistered'])
        self.assertFalse(result['checks']['allImagesDownloadedAndUnpacked'])

    def test_foreign_unpinned_duplicate_and_unknown_alias_sources_are_rejected(self):
        native=self.native['2022'];alias={'reference':native['alias'],'image':native['source']}
        for aliases in [[dict(alias,reference='other.example/image:tag')],[dict(alias,reference=native['source'])],[dict(alias,image=native['source']+'bad')],[alias,alias],[dict(alias,unknown=True)]]:
            with self.subTest(aliases=aliases),self.assertRaises(ValueError):make_recipe(self.runtime,[native['source']],'2022',aliases)
        self.assertEqual(make_recipe(self.runtime,[native['source']],'2022')['schemaVersion'],2)
