import copy
import hashlib
import gzip
import json
import pathlib
import unittest

from survivor_stream import evaluate


class NativeStreamTest(unittest.TestCase):
    fixtures = json.loads(gzip.decompress((pathlib.Path(__file__).parent / 'testdata/survivor-stream-native.json.gz').read_bytes()))

    def check(self, f, raw=None):
        return evaluate((raw if raw is not None else f['raw']).encode(), f['receipt'],
                        f['result'], f['sourceIP'], 10, f['progressAttempts'])

    def test_native_six_directions(self):
        self.assertEqual(sum(self.check(f)['attempts'] for f in self.fixtures), 1303)
        self.assertTrue(all(self.check(f)['ok'] for f in self.fixtures))

    def test_native_timeout_remains_failed_after_recovery(self):
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent / 'testdata/survivor-stream-native-failure.json.gz').read_bytes()))
        report=self.check(f)
        self.assertFalse(report['ok'])
        self.assertEqual((report['attempts'],report['passed']),(638,637))
        rows=[json.loads(line) for line in f['raw'].splitlines()]
        self.assertTrue(rows[-1]['ok'])
        self.assertFalse(rows[-1]['healthy'])

    def test_changed_and_truncated_bytes(self):
        for raw in (self.fixtures[0]['raw'][:-1], self.fixtures[0]['raw'] + '\n'):
            with self.assertRaises(ValueError):
                self.check(self.fixtures[0], raw)

    def test_rehashed_false_evidence(self):
        mutations = [lambda rows: rows[1].update(sequence=0),
                     lambda rows: rows[-1].update(stopBoundary=False),
                     lambda rows: rows[2].update(source='127.0.0.1:8081'),
                     lambda rows: rows[2].update(pid=1),
                     lambda rows: rows[2].update(startOffsetSeconds=-1),
                     lambda rows: rows[2].update(healthy=False),
                     lambda rows: rows[2].update(passed=0)]
        for mutate in mutations:
            f = copy.deepcopy(self.fixtures[0]); rows = [json.loads(x) for x in f['raw'].splitlines()]
            mutate(rows); f['raw'] = ''.join(json.dumps(r)+'\n' for r in rows)
            f['result']['samplesSHA256'] = hashlib.sha256(f['raw'].encode()).hexdigest()
            with self.assertRaises(ValueError): self.check(f)

    def test_missing_progress_and_wrong_terminal(self):
        for key, value in [('stopAcknowledged', False), ('passed', 0), ('pid', 1), ('ok', False)]:
            f = copy.deepcopy(self.fixtures[0]); f['result'][key] = value
            with self.assertRaises(ValueError): self.check(f)
        for count in (0, self.fixtures[0]['result']['attempts']):
            f = copy.deepcopy(self.fixtures[0]); f['progressAttempts'] = count
            with self.assertRaises(ValueError): self.check(f)


if __name__ == '__main__': unittest.main()
