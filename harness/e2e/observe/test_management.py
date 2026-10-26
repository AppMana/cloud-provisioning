import hashlib
import json
import pathlib
import tempfile
import unittest
import subprocess
from types import SimpleNamespace
from unittest.mock import patch
from management import command, successful, complete, assess_window, observe, summarize


class ManagementTests(unittest.TestCase):
    def test_failed_response_is_retained_without_retry_or_passing(self):
        for timeout in (False, True):
            with self.subTest(timeout=timeout), tempfile.TemporaryDirectory() as temp:
                output = pathlib.Path(temp) / 'observation'
                body, stderr = b'[-]etcd failed\n', b'diagnostic\n' + b'x' * 5000
                def snapshot(*args):
                    (output / 'STOP').touch()
                    return []
                response = subprocess.CompletedProcess([], 1, body, stderr)
                effect = subprocess.TimeoutExpired([], 8, output=body, stderr=stderr) if timeout else None
                with patch('management.snapshot', side_effect=snapshot), patch(
                        'management.subprocess.run', return_value=response, side_effect=effect) as run:
                    result = observe(SimpleNamespace(bastion='probe', namespace='probe'),
                                     ['https://192.0.2.1:6443'], [], output)
                run.assert_called_once()
                argv = run.call_args.args[0]
                self.assertEqual(argv[:5], ['docker', 'exec', '-e', 'GOMAXPROCS=2', 'probe'])
                self.assertIn('--server=https://192.0.2.1:6443', argv)
                self.assertEqual(run.call_args.kwargs['timeout'], 8)
                self.assertEqual(run.call_args.kwargs['env']['GOMAXPROCS'], '2')
                row = json.loads((output / 'lane-00.jsonl').read_text())
                self.assertFalse(row['ok'])
                self.assertFalse(result['ok'])
                self.assertEqual(row['response']['returnCode'], None if timeout else 1)
                self.assertEqual(row['response']['stdout'], body.decode())
                self.assertEqual(row['response']['stderr'], stderr[:4096].decode())
                self.assertEqual(row['response']['stderrBytes'], len(stderr))

    def test_direct_api_and_body_checks(self):
        for op in ('api', 'exec', 'logs'):
            args = command('https://192.0.2.1:6443', 'probe', 'hc-node', op)
            self.assertIn('--server=https://192.0.2.1:6443', args)
            self.assertFalse(successful(op, b'ok', 1))
            self.assertTrue(successful(op, b'ok\n', 0))
        self.assertFalse(successful('exec', b'other', 0))
        self.assertFalse(successful('api', b'', 0))
        self.assertTrue(successful('logs', b'', 0))

    def test_incomplete_monitor_never_passes(self):
        self.assertTrue(complete([{'ok': True}], True, True, True))
        for args in (([], True, True, True), ([{'ok': False}], True, True, True),
                     ([{'ok': True}], False, True, True), ([{'ok': True}], True, False, True),
                     ([{'ok': True}], True, True, False)):
            self.assertFalse(complete(*args))

    def test_raw_window_verification(self):
        for mutation in ('none', 'loss', 'gap', 'identity', 'deadline', 'early-stop', 'truncated', 'reordered', 'forged-summary'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temp:
                p = pathlib.Path(temp)
                identity = dict(nonce='unique', pid=123)
                lane = dict(server='https://192.0.2.1:6443', pod='hc-node', operation='exec')
                rows = [dict(identity, sequence=i, ok=True, startMonotonic=float(i),
                             finishMonotonic=i + .1, startedAt=f'2026-01-01T00:00:0{i}+00:00',
                             finishedAt=f'2026-01-01T00:00:0{i}.100000+00:00', stopBoundary=i == 3) for i in range(4)]
                if mutation == 'loss': rows[2]['ok'] = False
                if mutation == 'gap': rows[3]['startMonotonic'] = 12.; rows[3]['finishMonotonic'] = 12.1
                if mutation == 'identity': rows[2]['pid'] = 999
                if mutation == 'deadline': rows[3]['stopBoundary'] = False
                if mutation == 'early-stop': rows[3]['startedAt'] = '2026-01-01T00:00:02+00:00'
                if mutation == 'reordered': rows[2]['sequence'] = 1
                raw = ''.join(json.dumps(r)+'\n' for r in rows).encode()
                if mutation == 'truncated': raw = raw[:-1]
                summary = dict(lane, **summarize(rows, 5), stopAcknowledged=True, sha256=hashlib.sha256(raw).hexdigest())
                if mutation == 'forged-summary': summary['attempts'] = 100
                (p/'lane-00.jsonl').write_bytes(raw)
                for name, value in {
                    'initial': dict(identity, lanes=[lane], maxGapSeconds=5),
                    'READY': dict(identity, readyAt='2026-01-01T00:00:01.200000+00:00'),
                    'result': dict(identity, lanes=[summary], identitiesStable=True, stopAcknowledged=True, ready=True, ok=True),
                }.items(): (p/(name+'.json')).write_text(json.dumps(value))
                if mutation == 'none':
                    self.assertTrue(assess_window(p, '2026-01-01T00:00:01.500000+00:00', '2026-01-01T00:00:02.500000+00:00')['ok'])
                else:
                    with self.assertRaises(ValueError):
                        assess_window(p, '2026-01-01T00:00:01.500000+00:00', '2026-01-01T00:00:02.500000+00:00')


if __name__ == '__main__':
    unittest.main()
