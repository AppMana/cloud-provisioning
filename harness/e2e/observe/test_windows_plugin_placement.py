import copy
import json
import pathlib
import unittest
from windows_plugin_placement import evaluate

class NativePluginPlacementTest(unittest.TestCase):
    def setUp(self):
        self.snapshot = json.loads((pathlib.Path(__file__).parent/'testdata/windows-plugin-placement-native.json').read_text())

    def test_native_build_profiles_and_ready_pods(self):
        result = evaluate(self.snapshot)
        self.assertTrue(result['passed'])
        self.assertEqual(len(result['assignments']), 3)

    def test_observed_hostname_profiles_do_not_qualify_replacements(self):
        self.snapshot['daemonsets'] = self.snapshot['historicalDaemonsets']
        result = evaluate(self.snapshot)
        self.assertFalse(result['checks']['buildSelectors'])
        self.assertFalse(result['passed'])

    def test_duplicate_registration_is_rejected(self):
        self.snapshot['pods']['items'].append(copy.deepcopy(self.snapshot['pods']['items'][0]))
        self.assertFalse(evaluate(self.snapshot)['checks']['oneReadyPluginPerNode'])

    def test_windows2025_runtime_mount_opt_in_is_required(self):
        ds = next(d for d in self.snapshot['daemonsets']['items'] if d['metadata']['name'].endswith('runtime-mounts'))
        ds['spec']['template']['spec']['containers'][0]['env'] = []
        self.assertFalse(evaluate(self.snapshot)['checks']['windows2025RuntimeMountsSkipped'])

    def test_unqualified_build_is_rejected(self):
        self.snapshot['nodes']['items'][0]['metadata']['labels']['node.kubernetes.io/windows-build'] = '10.0.99999'
        self.assertFalse(evaluate(self.snapshot)['checks']['oneReadyPluginPerNode'])

if __name__ == '__main__': unittest.main()
