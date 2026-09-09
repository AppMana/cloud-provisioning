import copy
import json
import pathlib
import unittest
from windows_cache_reuse import evaluate
from images.windows.cache import make_recipe


class CacheReuse(unittest.TestCase):
    def test_native_2022_canonical_plugin_cache_reuse(self):
        fixture = json.loads((pathlib.Path(__file__).with_name('testdata') /
            'windows-startup-cache-canonical-2022.json').read_text())
        result = evaluate(**fixture)
        self.assertTrue(result['completeStartupCache'])
        self.assertEqual(len(result['containers']), 10)
        self.assertTrue(all(c['cacheHitEvent'] for c in result['containers']))
        plugin = next(c for c in result['containers'] if c['container'] == 'plugin')
        canonical = plugin['image'].replace('index.docker.io/', 'docker.io/', 1)
        self.assertNotEqual(canonical, plugin['image'])
        self.assertEqual(fixture['native']['imageTargets'][canonical],
                         fixture['native']['imageTargets'][plugin['image']])
        fixture['native']['readyImages'].remove(canonical)
        self.assertFalse(evaluate(**fixture)['completeStartupCache'])

    def test_native_2022_plugin_pull_prevents_complete_cache_qualification(self):
        fixture = json.loads((pathlib.Path(__file__).with_name('testdata') /
            'windows-startup-cache-missing-plugin-2022.json').read_text())
        result = evaluate(**fixture)
        self.assertFalse(result['completeStartupCache'])
        self.assertTrue(result['checks']['declaredReferencesReady'])
        missing = [c for c in result['containers'] if not c['declared']]
        self.assertEqual(len(missing), 1)
        self.assertEqual(missing[0]['container'], 'plugin')
        self.assertTrue(missing[0]['pullingEvent'])
        self.assertFalse(missing[0]['cacheHitEvent'])
        self.assertEqual(sum(c['cacheHitEvent'] for c in result['containers']), 9)

    def setUp(self):
        self.native=json.loads((pathlib.Path(__file__).with_name('testdata')/'windows-cache-reuse-native.json').read_text())

    def test_native_cache_hits_do_not_hide_uncached_pause(self):
        result=evaluate(**self.native)
        self.assertTrue(result['declaredCacheReused'])
        self.assertFalse(result['completeStartupCache'])
        self.assertEqual(len(result['containers']),10)
        self.assertEqual(result['undeclaredRuntimeImageTargets'],[
            'sha256:278fb9dbcca9518083ad1e11276933a2e96f23de604a3a08cc3c80002767d24c'])

    def test_fresh_native_worker_reuses_complete_startup_cache(self):
        fixture=json.loads((pathlib.Path(__file__).with_name('testdata')/
            'windows-startup-cache-reuse-native.json').read_text())
        result=evaluate(**fixture)
        self.assertTrue(result['completeStartupCache'])
        self.assertEqual(len(result['containers']),10)
        self.assertEqual(result['undeclaredRuntimeImageTargets'],[])
        # Observed first-GPU startup includes runtime pause, not just Pod images.
        pause='registry.k8s.io/pause@sha256:278fb9dbcca9518083ad1e11276933a2e96f23de604a3a08cc3c80002767d24c'
        fixture['native']['readyImages'].remove(pause)
        self.assertFalse(evaluate(**fixture)['completeStartupCache'])

    def test_missing_stale_or_other_node_static_events_fail(self):
        for mode in ['missing','stale','other-node']:
            fixture=copy.deepcopy(self.native)
            if mode=='missing':fixture['events']['items']=[]
            elif mode=='stale':
                for event in fixture['events']['items']:event['involvedObject']['uid']='old-pod-uid'
            else:
                mirror=next(p for p in fixture['pods']['items'] if p['metadata']['annotations'].get('kubernetes.io/config.mirror'))
                mirror['metadata']['ownerReferences'][0]['uid']='old-node-uid'
            with self.subTest(mode=mode):self.assertFalse(evaluate(**fixture)['declaredCacheReused'])

    def test_pulling_event_runtime_drift_and_unready_content_fail(self):
        for mode in ['pulling','runtime','unready','target','node']:
            fixture=copy.deepcopy(self.native)
            if mode=='pulling':
                event=copy.deepcopy(next(e for e in fixture['events']['items'] if e['reason']=='Pulled'));event['reason']='Pulling';fixture['events']['items'].append(event)
            elif mode=='runtime':fixture['native']['runtimeSHA256']='0'*64
            elif mode=='unready':fixture['native']['readyImages'].remove(fixture['recipe']['images'][0])
            elif mode=='target':fixture['native']['imageTargets'][fixture['recipe']['images'][0]]='sha256:'+'0'*64
            else:fixture['node_uid']='replacement-node'
            with self.subTest(mode=mode):self.assertFalse(evaluate(**fixture)['declaredCacheReused'])

    def test_simulated_complete_recipe_accounts_for_internal_pause_alias(self):
        # Models the next recipe only; the observed AMI did not bake pause.
        fixture=copy.deepcopy(self.native);old=fixture['recipe'];pause='registry.k8s.io/pause@sha256:278fb9dbcca9518083ad1e11276933a2e96f23de604a3a08cc3c80002767d24c'
        fixture['recipe']=make_recipe(old['runtime'],old['images']+[pause],'2025',old['aliases']+[{'reference':'registry.k8s.io/pause:3.10.1','image':pause}])
        self.assertTrue(evaluate(**fixture)['completeStartupCache'])
