import copy
import json
import pathlib
import unittest

from gateway_withdrawal import evaluate


class GatewayWithdrawal(unittest.TestCase):
    def setUp(self):
        self.fixture = json.loads((pathlib.Path(__file__).with_name('testdata') /
                                   'gateway-withdrawal-native.json').read_text())

    def check(self, rows):
        return evaluate(rows, self.fixture['secretUID'])

    def test_native_changed_document_ack_precedes_global_withdrawal(self):
        result = self.check(self.fixture['rows'])
        self.assertTrue(result['passed'])
        self.assertEqual(result['firstNewAcknowledgementAt'], '2026-09-08T13:48:35.869132+00:00')
        self.assertEqual(result['globalWithdrawalAt'], '2026-09-08T13:48:44.258476+00:00')

    def test_native_windows2022_reusable_capture(self):
        fixture = json.loads((pathlib.Path(__file__).with_name('testdata') /
                              'gateway-withdrawal-windows2022-native.json').read_text())
        result = evaluate(fixture['rows'], fixture['secretUID'])
        self.assertTrue(result['passed'])
        self.assertEqual(result['firstNewAcknowledgementAt'], '2026-09-08T14:34:27.464720+00:00')
        self.assertEqual(result['globalWithdrawalAt'], '2026-09-08T14:34:53.095936+00:00')

    def test_old_self_consistent_ack_cannot_qualify_staging(self):
        rows = copy.deepcopy(self.fixture['rows'])
        old = rows[0]['workerDocumentSHA256']
        for row in rows:
            if row['retiringWorker']:
                row['workerDocumentSHA256'] = old
                row['workerAppliedSHA256'] = old
        result = self.check(rows)
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['workerAcknowledgedBeforeGlobalWithdrawal'])

    def test_mixed_snapshots_and_replacements_fail(self):
        for mode in ['mesh', 'secret', 'unstable', 'unready', 'reappeared', 'incomplete']:
            rows = copy.deepcopy(self.fixture['rows'])
            if mode == 'mesh':
                rows[1]['meshUID'] = 'replacement'
            elif mode == 'secret':
                rows[1]['workerSecretUID'] = 'replacement'
            elif mode == 'unstable':
                for i, row in enumerate(rows):
                    row['meshResourceVersion'] = str(i)
            elif mode == 'unready':
                rows[1]['nodeReady'] = False
            elif mode == 'reappeared':
                rows[-1]['projectionPresent'] = True
            else:
                rows[-1]['machinePresent'] = True
            with self.subTest(mode=mode):
                self.assertFalse(self.check(rows)['passed'])

    def test_unordered_or_unbound_samples_are_rejected(self):
        with self.assertRaises(ValueError):
            evaluate([], self.fixture['secretUID'])
        with self.assertRaises(ValueError):
            evaluate(self.fixture['rows'], '')
        with self.assertRaises(ValueError):
            self.check(list(reversed(self.fixture['rows'])))
