import datetime
import json
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

from survivor_native import Session, fresh_progress
from survivor_oob import OutOfBand


class FreshnessTest(unittest.TestCase):
    def test_age_includes_executor_queue_and_rejects_missing_clock_binding(self):
        now = datetime.datetime.fromisoformat('2026-10-15T05:15:17+00:00')
        row = dict(requestedAt='2026-10-15T05:15:12+00:00', observedAt='2026-10-15T05:15:16+00:00')
        self.assertTrue(fresh_progress([row], 10, now))
        for changes in (dict(requestedAt='2026-10-15T05:14:20+00:00'),
                        dict(observedAt='2026-10-15T05:15:18+00:00'),
                        dict(requestedAt='2026-10-15T05:15:12'), dict(requestedAt='invalid')):
            self.assertFalse(fresh_progress([dict(row, **changes)], 10, now))
        self.assertFalse(fresh_progress([{'observedAt':row['observedAt']}], 10, now))

    def test_ready_rejects_old_healthy_scan(self):
        with tempfile.TemporaryDirectory() as tmp:
            p=pathlib.Path(tmp)
            (p/'session.json').write_text(json.dumps(dict(lanes=[{}],maxGap=10)))
            for name,count,stamp in [('first',10,'2026-01-01T00:00:00+00:00'),('second',11,'2026-01-01T00:00:01+00:00')]:
                (p/f'progress-{name}.json').write_text(json.dumps([dict(nonce='same',pid=1,attempts=count,healthy=True,requestedAt=stamp,observedAt=stamp)]))
            with self.assertRaises(ValueError):Session(Mock(),p).ready('first','second')
            self.assertFalse((p/'READY.json').exists())

    def test_ready_accepts_complete_fresh_advancing_scan(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = pathlib.Path(tmp)
            (p/'session.json').write_text(json.dumps(dict(lanes=[{}], maxGap=10)))
            now = datetime.datetime.now(datetime.timezone.utc)
            for name, count, age in [('first', 10, 2), ('second', 11, 1)]:
                stamp = (now - datetime.timedelta(seconds=age)).isoformat()
                row = dict(nonce='same', pid=1, attempts=count, healthy=True,
                           requestedAt=stamp, observedAt=stamp)
                (p/f'progress-{name}.json').write_text(json.dumps([row]))
            self.assertEqual(Session(Mock(), p).ready('first', 'second')['lanes'], 1)
            self.assertTrue((p/'READY.json').exists())

    def test_linux_status_batch_uses_literal_argument_paths(self):
        class Local(OutOfBand):
            def exec_result(self, source, arguments, timeout=30):
                r=subprocess.run(arguments,capture_output=True,timeout=timeout)
                return r.stdout,r.returncode,r.stderr.decode()
        with tempfile.TemporaryDirectory() as tmp:
            p=pathlib.Path(tmp);exe=p/'probe with spaces'
            exe.write_text('#!/bin/sh\n[ "$1" = "-stream-dir" ] && [ "$3" = "-status" ] || exit 3\nprintf \'{"terminal":false}\\n\'\n');exe.chmod(0o700)
            k=Local('https://test','bastion','ns',{'uid':dict(os='linux',command=['unused'])})
            body,code,_=k.status_many({'os':'linux'},[(str(exe),'dir one'),(str(exe),'literal $(no-command)')])
            self.assertEqual(code,0)
            self.assertEqual(json.loads(body),[{'terminal':False},{'terminal':False}])

    def test_grouped_progress_binds_each_native_nonce(self):
        class Batch:
            def status_many(self, source, entries):
                statuses=[dict(terminal=False,latest=dict(nonce=n,pid=i+10,attempts=5,passed=5,healthy=True,maxStartIntervalSeconds=.1)) for i,n in enumerate(self.nonces)]
                return json.dumps(statuses).encode(),0,''
        for nonces,good in [(['a','b'],True),(['b','a'],False),(['a'],False)]:
            with self.subTest(nonces=nonces),tempfile.TemporaryDirectory() as tmp:
                p=pathlib.Path(tmp);k=Batch();k.nonces=nonces
                lanes=[dict(source=dict(nodeUID='uid',os='linux'),executable='probe',directory='dir'+str(i)) for i in range(2)]
                (p/'session.json').write_text(json.dumps(dict(lanes=lanes,maxGap=10)))
                for i,n in enumerate(['a','b']):(p/f'lane-{i:02d}-receipt.json').write_text(json.dumps(dict(nonce=n,pid=i+10)))
                if good:
                    rows=Session(k,p).progress('check');self.assertTrue(fresh_progress(rows,10));self.assertEqual([x['nonce'] for x in rows],nonces)
                else:
                    with self.assertRaises(ValueError):Session(k,p).progress('check')
