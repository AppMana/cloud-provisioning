import contextlib
import io
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import udp


class UDPResultTest(unittest.TestCase):
    def test_native_windows_timeout_fixture_fails_with_successful_curl_exit(self):
        body = (pathlib.Path(__file__).parent / 'testdata' /
                'agnhost-windows-udp-timeout.json').read_text()
        result = udp.evaluate(body, 'x' * 1400, 1, 0)
        self.assertFalse(result['ok'])
        self.assertFalse(result['responsesPresent'])
        self.assertEqual(result['reportedResponses'], 0)
        self.assertIn('i/o timeout', result['errors'][0])

    def test_exact_echoes_required_even_when_transport_succeeds(self):
        self.assertTrue(udp.evaluate('{"responses":["xxx","xxx"]}', 'xxx', 2)['ok'])
        for body in ['{"responses":["xxx"]}', '{"responses":["XXX","xxx"]}',
                     '{"responses":["xxx","xxx","xxx"]}',
                     '{"responses":["xxx","xxx"],"errors":["timeout"]}']:
            self.assertFalse(udp.evaluate(body, 'xxx', 2)['ok'])
        self.assertFalse(udp.evaluate('{"responses":["xxx"]}', 'xxx', 1, 22)['ok'])

    def test_last_failure_does_not_invent_responses_or_an_exact_loss_count(self):
        result = udp.evaluate('{"errors":["i/o timeout"]}', 'xxx', 50)
        self.assertFalse(result['ok'])
        self.assertFalse(result['responsesPresent'])
        self.assertEqual(result['reportedResponses'], 0)
        self.assertEqual(result['errors'], ['i/o timeout'])
        self.assertNotIn('packetsLost', result)

    def test_malformed_null_and_non_array_results_fail_closed(self):
        for body in ['', 'null', '[]', '{"responses":null}', '{"responses":"xxx"}',
                     '{"responses":[null]}', '{"errors":[null]}', '{"errors":{}}']:
            result = udp.evaluate(body, 'xxx', 1)
            self.assertFalse(result['ok'])
            self.assertIn('parseError', result)
        with self.assertRaises(ValueError):
            udp.evaluate('{}', 'xxx', 0)

    def test_bom_and_partial_native_shaped_result(self):
        result = udp.evaluate('\ufeff' + json.dumps({
            'responses': ['x' * 1400] * 48,
            'errors': ['reading from udp connection failed. err: i/o timeout'] * 2,
        }), 'x' * 1400, 50)
        self.assertFalse(result['ok'])
        self.assertEqual(result['reportedResponses'], 48)
        self.assertEqual(result['exactResponses'], 48)

    def test_cli_replay_and_executor_exit_status(self):
        with tempfile.TemporaryDirectory() as directory:
            body = pathlib.Path(directory) / 'body.json'
            body.write_text('{"responses":["xxx"]}')
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(udp.main(['--payload-bytes', '3', '--tries', '1',
                                           '--body', str(body)]), 0)
                self.assertEqual(udp.main(['--payload-bytes', '3', '--tries', '1',
                                           '--body', str(body), '--exit-code', '22']), 1)
        command = ['--payload-bytes', '3', '--tries', '1', '--', 'executor', 'curl']
        with contextlib.redirect_stdout(io.StringIO()), patch('udp.subprocess.run') as run:
            run.return_value = subprocess.CompletedProcess(['executor'], 0, '{"responses":["xxx"]}')
            self.assertEqual(udp.main(command), 0)
            self.assertEqual(run.call_args.args[0], ['executor', 'curl'])
            run.side_effect = subprocess.TimeoutExpired('executor', 90)
            self.assertEqual(udp.main(command), 1)
            run.side_effect = FileNotFoundError()
            self.assertEqual(udp.main(command), 1)

    def test_cli_rejects_ambiguous_or_invalid_inputs(self):
        for args in [
            ['--payload-bytes', '2044', '--tries', '1', '--body', 'unused'],
            ['--payload-bytes', '3', '--tries', '0', '--body', 'unused'],
            ['--payload-bytes', '3', '--tries', '1'],
            ['--payload-bytes', '3', '--tries', '1', '--body', 'unused', '--', 'executor'],
            ['--payload-bytes', '3', '--tries', '1', '--exit-code', '22', '--', 'executor'],
        ]:
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as error:
                udp.main(args)
            self.assertEqual(error.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
