import base64
import copy
import json
import pathlib
import tempfile
import time
import unittest
from unittest.mock import patch

import gateway_capture


class GatewayCapture(unittest.TestCase):
    def setUp(self):
        self.native = json.loads((pathlib.Path(__file__).with_name('testdata') /
                                  'gateway-withdrawal-native.json').read_text())
        self.binding = dict(machine='aws-win2025-gpu-withdraw-v1',
                            machineUID='e819b357-9906-483d-9bbf-9e652a8c4f82',
                            node='aws-win2025-gpu-withdraw-v1',
                            nodeUID='14c798d3-b8fe-4ac4-9a3f-9569abd352a4',
                            meshUID=self.native['rows'][0]['meshUID'],
                            secretUID=self.native['secretUID'], lease='original-lease')

    def test_captures_original_native_sequence_and_validates_changed_ack(self):
        with tempfile.TemporaryDirectory() as work:
            output = pathlib.Path(work) / 'capture'
            with patch.object(gateway_capture, 'sample', side_effect=self.native['rows']):
                result = gateway_capture.capture(None, self.binding, output, time.monotonic() + 30, interval=0)
            self.assertTrue(result['passed'])
            self.assertEqual([json.loads(line) for line in (output / 'samples.jsonl').read_text().splitlines()],
                             self.native['rows'])
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            # A retry cannot overwrite or append to the original observation.
            with self.assertRaises(FileExistsError):
                gateway_capture.capture(None, self.binding, output, time.monotonic() + 30)

    def test_late_start_and_deadline_retain_evidence_without_pass_result(self):
        with tempfile.TemporaryDirectory() as work:
            output = pathlib.Path(work) / 'late'
            staged = next(row for row in self.native['rows'] if row['retiringWorker'])
            with patch.object(gateway_capture, 'sample', return_value=staged), self.assertRaises(ValueError):
                gateway_capture.capture(None, self.binding, output, time.monotonic() + 30)
            self.assertEqual(json.loads((output / 'samples.jsonl').read_text()), staged)
            self.assertFalse((output / 'result.json').exists())
            output = pathlib.Path(work) / 'expired'
            with self.assertRaises(TimeoutError):
                gateway_capture.capture(None, self.binding, output, time.monotonic() - 1)
            self.assertFalse((output / 'result.json').exists())

    def objects(self):
        # Minimal Kubernetes envelopes simulate the public fields read by the
        # native sampler. Payload bytes are synthetic, not captured credentials.
        b = self.binding
        mesh = dict(metadata=dict(uid=b['meshUID'], resourceVersion='123'),
                    data={'gateway-projections.json': base64.b64encode(json.dumps([
                        dict(lease=b['lease'], retiringWorker=True)]).encode()).decode(),
                          'unused-private-field': 'DO-NOT-PUBLISH'})
        secret = dict(metadata=dict(uid=b['secretUID']),
                      data={'peers.json': base64.b64encode(b'{}').decode(), 'token': 'DO-NOT-PUBLISH'})
        machine = dict(metadata=dict(uid=b['machineUID'], deletionTimestamp='2026-09-08T13:48:00Z'))
        node = dict(metadata=dict(uid=b['nodeUID']), status=dict(conditions=[dict(type='Ready', status='True')]))
        return [mesh, secret, machine, node]

    def test_reads_mesh_before_ack_and_exports_only_hashes(self):
        objects = self.objects()
        calls = []
        def get(*args):
            calls.append(args)
            return objects[len(calls) - 1]
        result = gateway_capture.sample(get, self.binding)
        self.assertEqual([c[0] for c in calls], ['secret', 'secret', 'machine', 'node'])
        self.assertIsNone(calls[-1][2])
        self.assertTrue(result['retiringWorker'] and result['machineDeleting'] and result['nodeReady'])
        self.assertEqual(result['workerDocumentSHA256'],
                         '44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a')
        self.assertNotIn('DO-NOT-PUBLISH', json.dumps(result))

    def test_same_name_replacements_and_duplicate_lease_are_rejected(self):
        for index in range(4):
            objects = self.objects()
            objects[index]['metadata']['uid'] = 'replacement'
            with self.subTest(index=index), self.assertRaises(ValueError):
                gateway_capture.sample(lambda *args: objects.pop(0), self.binding)
        objects = self.objects()
        objects[0]['data']['gateway-projections.json'] = base64.b64encode(json.dumps([
            dict(lease=self.binding['lease']), dict(lease=self.binding['lease'])]).encode()).decode()
        with self.assertRaises(ValueError):
            gateway_capture.sample(lambda *args: objects.pop(0), self.binding)

    def test_terminal_absence_is_observed_but_missing_mesh_is_not_success(self):
        objects = self.objects()
        objects[0]['data']['gateway-projections.json'] = 'W10='
        objects[1:] = [None, None, None]
        result = gateway_capture.sample(lambda *args: objects.pop(0), self.binding)
        self.assertFalse(result['machinePresent'] or result['projectionPresent'] or result['nodeReady'])
        self.assertIsNone(result['workerSecretUID'])
        with self.assertRaises(ValueError):
            gateway_capture.sample(lambda *args: None, self.binding)

    def test_missing_original_identity_cannot_create_capture(self):
        with tempfile.TemporaryDirectory() as work:
            output = pathlib.Path(work) / 'capture'
            for key in self.binding:
                binding = copy.deepcopy(self.binding)
                del binding[key]
                with self.subTest(key=key), self.assertRaises(ValueError):
                    gateway_capture.capture(None, binding, output, time.monotonic() + 30)
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
