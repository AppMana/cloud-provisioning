import copy
import unittest
from unittest.mock import patch

import wireguard_handover_native as native


class IsolationGuardTest(unittest.TestCase):
    def test_host_network_and_mount_namespaces_are_refused_before_commands(self):
        for shared in ['net', 'mnt']:
            def identity(path):
                kind = path.rsplit('/', 1)[1]
                return kind if kind == shared else path
            with self.subTest(shared=shared), patch.object(native.os, 'readlink', side_effect=identity), patch.object(native.subprocess, 'check_output') as command:
                with self.assertRaises(RuntimeError):
                    native.require_isolation()
                command.assert_not_called()

    def test_nonempty_network_is_refused_before_any_mutation(self):
        with patch.object(native.os, 'readlink', side_effect=lambda path: path), patch.object(native.subprocess, 'check_output', return_value=b'[{"ifname":"lo"},{"ifname":"eth0"}]') as command:
            with self.assertRaises(RuntimeError):
                native.require_isolation()
            command.assert_called_once_with(['ip', '-j', 'link'])

    def test_missing_tool_is_refused_before_any_mutation(self):
        with patch.object(native.os, 'readlink', side_effect=lambda path: path), patch.object(native.subprocess, 'check_output', return_value=b'[{"ifname":"lo"}]') as command, patch.object(native.shutil, 'which', return_value=None):
            with self.assertRaises(RuntimeError):
                native.require_isolation()
            command.assert_called_once_with(['ip', '-j', 'link'])


class OutcomeContractTest(unittest.TestCase):
    def test_expected_loss_cannot_hide_missing_or_invalid_measurements(self):
        success = dict(ok=True, receiverObservedSource='10.90.0.10', replySource='10.88.0.3')
        failure = dict(ok=False, error='timeout')
        phases = {name: [copy.deepcopy(success if 'both-directions' in name else failure) for _ in range(3)]
                  for name in ['both-directions-old', 'old-outbound-new-return',
                               'both-directions-new', 'new-outbound-old-return']}
        self.assertTrue(native.outcomes_expected(phases, 'single'))
        self.assertFalse(native.outcomes_expected(phases, 'dual'))
        for fault in ['missing', 'source', 'reply', 'error', 'count']:
            changed = copy.deepcopy(phases)
            if fault == 'missing':
                del changed['both-directions-old']
            elif fault == 'source':
                changed['both-directions-old'][0]['receiverObservedSource'] = '10.90.0.1'
            elif fault == 'reply':
                changed['both-directions-old'][0]['replySource'] = '10.88.0.2'
            elif fault == 'error':
                changed['old-outbound-new-return'][0]['error'] = 'invalid response'
            else:
                changed['old-outbound-new-return'].pop()
            with self.subTest(fault=fault):
                self.assertFalse(native.outcomes_expected(changed, 'single'))


if __name__ == '__main__':
    unittest.main()
