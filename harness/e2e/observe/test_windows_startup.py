import copy
import json
import pathlib
import unittest
from windows_startup import evaluate

class StartupTest(unittest.TestCase):
    def setUp(self):
        self.snapshot=json.loads((pathlib.Path(__file__).parent/'testdata/windows-startup-misplaced-nfd.json').read_text())
        self.node=self.snapshot['node'];self.pods=self.snapshot['pods'];self.uid=self.node['metadata']['uid']

    def test_native_unstarted_nfd_invalidates_first_application_guard(self):
        result=evaluate(self.node,self.pods,self.uid)
        self.assertFalse(result['passed'])
        self.assertTrue(any('node-feature-discovery' in c['pod'] for c in result['ordinaryContainers']))

    def clean(self):
        self.pods['items']=[p for p in self.pods['items'] if 'node-feature-discovery' not in p['metadata']['name']]

    def test_native_hostprocess_only_inventory(self):
        self.clean();self.assertTrue(evaluate(self.node,self.pods,self.uid)['passed'])

    def test_one_hostprocess_container_cannot_hide_ordinary_init_or_debug_container(self):
        self.clean()
        for kind in ['initContainers','ephemeralContainers','containers']:
            with self.subTest(kind=kind):
                pods=copy.deepcopy(self.pods);p=pods['items'][0]
                p['spec'].setdefault(kind,[]).append({'name':'ordinary','securityContext':{'windowsOptions':{'hostProcess':False}}})
                self.assertFalse(evaluate(self.node,pods,self.uid)['passed'])

    def test_replaced_or_unready_node_is_rejected(self):
        self.clean();self.assertFalse(evaluate(self.node,self.pods,'different')['passed'])
        self.node['metadata']['deletionTimestamp']='2026-09-08T00:00:00Z'
        self.assertFalse(evaluate(self.node,self.pods,self.uid)['passed'])

    def test_other_nodes_do_not_contaminate_inventory(self):
        for pod in self.pods['items']:pod['spec']['nodeName']='another-node'
        self.assertTrue(evaluate(self.node,self.pods,self.uid)['passed'])

    def test_native_plugin_readiness_precedes_reported_gpu_capacity(self):
        data=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-resource-registration-2025.json').read_text())
        before,after=data['before'],data['after']
        pods={'items':[]}  # Unit-only first-workload inventory, not a native after-run claim.
        uid=before['metadata']['uid']
        self.assertTrue(evaluate(before,pods,uid)['passed'])
        result=evaluate(before,pods,uid,require_gpu=True)
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['gpuReportedAllocatable'])
        self.assertTrue(evaluate(after,pods,uid,require_gpu=True)['passed'])

    def test_gpu_capacity_must_be_positive_integer_and_consistent(self):
        data=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-resource-registration-2025.json').read_text())
        for capacity,allocatable in [('1','0'),('1','2'),('1',''),('1',True),('1','0.5'),('', '1')]:
            node=copy.deepcopy(data['after'])
            node['status']['capacity']['directx.microsoft.com/display']=capacity
            node['status']['allocatable']['directx.microsoft.com/display']=allocatable
            self.assertFalse(evaluate(node,{'items':[]},node['metadata']['uid'],require_gpu=True)['passed'])
