import copy
import json
import pathlib
import unittest
from windows_process import evaluate
from windows_gpu import GPU


class ProcessControlTest(unittest.TestCase):
    def setUp(self):
        self.f = json.loads((pathlib.Path(__file__).parent/'testdata/windows-process-no-gpu.json').read_text())
        self.ids = [self.f[k]['metadata']['uid'] for k in ['node','pod','job']]

    def run_control(self):
        f = self.f
        return evaluate(f['node'], f['pod'], f['job'], *self.ids, f['events'])

    def test_candidate_image_is_not_inferred_from_the_pod(self):
        f = self.f
        candidate = 'example.invalid/probe@sha256:' + 'b'*64
        f['pod']['spec']['containers'][0]['image'] = candidate
        with self.assertRaises(ValueError):self.run_control()
        result = evaluate(f['node'], f['pod'], f['job'], *self.ids, f['events'], image=candidate)
        self.assertEqual(result['image'], candidate)
        with self.assertRaises(ValueError):
            evaluate(f['node'], f['pod'], f['job'], *self.ids, f['events'], image='probe:latest')

    def test_native_process_started_although_gpu_probe_failed(self):
        r = self.run_control()
        self.assertTrue(r['processCreated'])
        self.assertEqual(r['processCreationObservation'], 'observed-success')
        self.assertEqual(r['createProcessResults'], [{'result': '0x00000000', 'processID': 2872}])
        self.assertEqual(self.f['pod']['status']['containerStatuses'][0]['state']['terminated']['exitCode'], 1)

    def test_identity_and_assignment_boundaries(self):
        changes = {
            'replacement node': lambda f: f['node']['metadata'].update(uid='other'),
            'replacement job': lambda f: f['job']['metadata'].update(uid='other'),
            'unrelated owner': lambda f: f['pod']['metadata']['ownerReferences'][0].update(uid='other'),
            'host process': lambda f: f['pod']['spec'].update(securityContext={'windowsOptions': {'hostProcess': True}}),
            'gpu limit even zero': lambda f: f['pod']['spec']['containers'][0].update(resources={'limits': {GPU:'0'}}),
            'gpu request': lambda f: f['pod']['spec']['containers'][0].update(resources={'requests': {GPU:'1'}}),
            'different command': lambda f: f['pod']['spec']['containers'][0].update(command=['cmd.exe']),
            'missing native specification': lambda f: f.update(events=[e for e in f['events'] if e['id'] != 2010]),
        }
        original = copy.deepcopy(self.f)
        for name, change in changes.items():
            with self.subTest(name=name):
                self.f = copy.deepcopy(original)
                change(self.f)
                with self.assertRaises(ValueError):
                    self.run_control()

    def test_native_device_assignment_cannot_be_hidden_by_pod_spec(self):
        e = next(e for e in self.f['events'] if e['id']==2010)
        e['message'] = e['message'].replace('"AssignedDevices": []', '"AssignedDevices": [{"Type": "DeviceInstance"}]')
        with self.assertRaises(ValueError):
            self.run_control()

    def test_later_success_cannot_hide_an_earlier_process_attempt(self):
        event = copy.deepcopy(next(e for e in self.f['events'] if e['id']==2500))
        event['message'] = event['message'].replace('result 0x00000000, process ID 2872', 'result 0x800706BA, process ID 0')
        self.f['events'].append(event)
        with self.assertRaises(ValueError):
            self.run_control()

    def test_missing_process_event_is_unobserved(self):
        self.f['events'] = [e for e in self.f['events'] if e['id'] != 2500]
        r = self.run_control()
        self.assertFalse(r['processCreated'])
        self.assertEqual(r['processCreationObservation'], 'unobserved')

    def test_explicit_rpc_failure_is_distinct_from_missing_evidence(self):
        e = next(e for e in self.f['events'] if e['id']==2500)
        e['message'] = e['message'].replace('result 0x00000000, process ID 2872', 'result 0x800706BA, process ID 0')
        r = self.run_control()
        self.assertFalse(r['processCreated'])
        self.assertEqual(r['processCreationObservation'], 'observed-failure')


if __name__ == '__main__':
    unittest.main()
