"""Regression cases derived from native Windows GPU Jobs, including failures."""
import copy
import json
import pathlib
import unittest
import tempfile
from windows_gpu import evaluate, write_observation


class WindowsGPUObservation(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-pending.json').read_text())

    def observe(self, fixture=None, **kwargs):
        f = fixture or self.fixture
        return evaluate(f['node'], f['pod'], f['job'],
                        kwargs.get('node_uid', f['node']['metadata']['uid']),
                        kwargs.get('pod_uid', f['pod']['metadata']['uid']),
                        f.get('logs', ''), f.get('probeName', 'windows-gpu-probe'))

    def test_candidate_image_requires_explicit_immutable_identity(self):
        f = copy.deepcopy(self.fixture)
        candidate = 'example.invalid/probe@sha256:' + 'a'*64
        f['pod']['spec']['containers'][0]['image'] = candidate
        with self.assertRaises(ValueError):
            self.observe(f)
        result = evaluate(f['node'], f['pod'], f['job'], f['node']['metadata']['uid'],
                          f['pod']['metadata']['uid'], image=candidate,
                          probe_name=f.get('probeName', 'windows-gpu-probe'))
        self.assertFalse(result['passed'])
        self.assertEqual(result['image'], candidate)
        with self.assertRaises(ValueError):
            evaluate(f['node'], f['pod'], f['job'], f['node']['metadata']['uid'],
                     f['pod']['metadata']['uid'], image='example.invalid/probe:latest')

    def test_native_render_encode_success_requires_all_evidence(self):
        f=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-success.json').read_text())
        self.assertTrue(self.observe(f)['passed'])
        receipt=json.loads(f['logs'])
        for field, value in [('hardwareOnly', False), ('vendorID', 0), ('frames', 29), ('greenPixels', 0)]:
            changed=copy.deepcopy(f)
            modified=copy.deepcopy(receipt)
            modified['render'][field]=value
            changed['logs']=json.dumps(modified)
            with self.subTest(field=field):
                self.assertFalse(self.observe(changed)['passed'])
        for field, value in [('decodedFrames', 29), ('encoder', 'libx264'), ('encodedBytes', 0)]:
            changed=copy.deepcopy(f)
            changed['logs']=json.dumps(dict(receipt, **{field:value}))
            with self.subTest(field=field):
                self.assertFalse(self.observe(changed)['passed'])
        changed=copy.deepcopy(f)
        changed['logs']=''
        self.assertFalse(self.observe(changed)['passed'])

    def test_later_success_cannot_overwrite_recorded_start_failure(self):
        directory = pathlib.Path(__file__).parent/'testdata'
        failure = self.observe(json.loads((directory/'windows-gpu-start-error.json').read_text()))
        success = self.observe(json.loads((directory/'windows-gpu-success.json').read_text()))
        self.assertFalse(failure['passed'])
        self.assertTrue(success['passed'])
        with tempfile.TemporaryDirectory() as temp:
            path = pathlib.Path(temp)/'observation.json'
            write_observation(path, failure)
            with self.assertRaises(FileExistsError):
                write_observation(path, success)
            self.assertEqual(json.loads(path.read_text()), failure)

    def test_another_probe_revision_is_not_accepted(self):
        f=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-success.json').read_text())
        f['probeName']='windows-gpu-probe'
        with self.assertRaises(ValueError):
            self.observe(f)

    def test_pending_gpu_reservation_is_not_workload_success(self):
        result = self.observe()
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['hardwareRenderAndEncode'])
        self.assertIsNone(result['receipt'])

    def test_healthy_allocated_device_does_not_hide_native_start_error(self):
        f=json.loads((pathlib.Path(__file__).parent/'testdata/windows-gpu-start-error.json').read_text())
        result=self.observe(f)
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['containerExitedZero'])
        self.assertEqual(result['containerStatus']['state']['terminated']['reason'],'StartError')
        self.assertEqual(result['containerStatus']['allocatedResources']['directx.microsoft.com/display'],'1')

    def test_replaced_node_or_pod_is_rejected(self):
        for changed in ['node_uid', 'pod_uid']:
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                self.observe(**{changed: 'replacement'})

    def test_hostprocess_or_hostnetwork_diagnostics_cannot_qualify_application(self):
        for context in ['host', 'pod', 'container']:
            f = copy.deepcopy(self.fixture)
            if context == 'host':
                f['pod']['spec']['hostNetwork'] = True
            else:
                target = f['pod']['spec'] if context == 'pod' else f['pod']['spec']['containers'][0]
                target['securityContext'] = {'windowsOptions': {'hostProcess': True}}
            with self.subTest(context=context), self.assertRaises(ValueError):
                self.observe(f)

    def test_missing_device_request_and_substituted_command_are_rejected(self):
        for change in ['resource', 'command', 'image', 'owner']:
            f = copy.deepcopy(self.fixture)
            container = f['pod']['spec']['containers'][0]
            if change == 'resource':container['resources'] = {}
            elif change == 'command':container['command'] = ['echo', 'fake result']
            elif change == 'image':container['image'] = 'unqualified-image'
            else:f['pod']['metadata']['ownerReferences'] = []
            with self.subTest(change=change), self.assertRaises(ValueError):
                self.observe(f)


if __name__ == '__main__':
    unittest.main()
