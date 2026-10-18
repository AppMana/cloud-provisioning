import json
import pathlib
import tempfile
import unittest
from unittest.mock import Mock

from survivor_native import Session


class NativeControlTest(unittest.TestCase):
    def test_slow_identity_reads_do_not_age_final_progress(self):
        import datetime
        from unittest.mock import patch
        from survivor_native import fresh_progress
        initial = [dict(pod='a', nodeUID='a'), dict(pod='b', nodeUID='b')]
        for fault in [None, 'changed-identity', 'stalled', 'stale-progress']:
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as root:
                p = pathlib.Path(root)
                session = Session(Mock(), p)
                (p/'initial.json').write_text(json.dumps(initial))
                (p/'session.json').write_text(json.dumps(dict(maxGap=5, lanes=[
                    dict(source=s, target=t) for s in initial for t in initial if s != t])))
                clock = [datetime.datetime(2026, 10, 18, tzinfo=datetime.timezone.utc)]
                snapshots = []
                rounds = []
                def snapshot(*args, **kwargs):
                    clock[0] += datetime.timedelta(seconds=6)
                    snapshots.append(clock[0])
                    return [] if fault == 'changed-identity' and len(snapshots) == 2 else initial
                def progress(label):
                    rounds.append(label)
                    stamp = clock[0].isoformat()
                    rows = [dict(nonce=str(i), pid=i+1,
                                 attempts=1 if fault == 'stalled' else len(rounds),
                                 requestedAt=stamp, observedAt=stamp) for i in range(2)]
                    (p/('progress-'+label+'.json')).write_text(json.dumps(rows))
                    if fault == 'stale-progress':
                        clock[0] += datetime.timedelta(seconds=6)
                    return rows
                with patch('pod_matrix.snapshot', side_effect=snapshot), patch('time.sleep'), \
                        patch.object(session, 'progress', side_effect=progress), \
                        patch('survivor_native.fresh_progress', side_effect=lambda rows, age: fresh_progress(rows, age, clock[0])):
                    self.assertEqual(session.live_ready(['a=a', 'b=b'], 'baseline'), fault is None)
                self.assertEqual((p/'READY.json').exists(), fault is None)

    def test_ipv6_session_preserves_family_when_resuming_identity_checks(self):
        from unittest.mock import patch
        from pod_matrix import snapshot
        from test_pod_matrix import DualStackCluster
        kube = DualStackCluster()
        sources = snapshot(kube, kube.targets, family=6)
        targets = [s['pod']+'='+s['nodeUID'] for s in sources]
        with tempfile.TemporaryDirectory() as root:
            output = pathlib.Path(root)/'streams'
            session = Session(kube, output)
            with patch.object(session, 'control', return_value={}) as control:
                session.start(sources, {s['nodeUID']: '/tmp/probe' for s in sources})
            for call in control.call_args_list:
                arguments = call.args[3]
                self.assertTrue(arguments[arguments.index('-destination')+1].startswith('[2001:db8::'))
                self.assertTrue(arguments[arguments.index('-source-ip')+1].startswith('2001:db8::'))
            self.assertEqual(json.loads((output/'session.json').read_text())['addressFamily'], 6)
            resumed = Session(kube, output)
            with patch('time.sleep'), patch('survivor_native.fresh_progress', return_value=True), \
                    patch.object(resumed, 'progress', side_effect=[[dict(attempts=1)]*6, [dict(attempts=2)]*6]), \
                    patch.object(resumed, 'ready'):
                self.assertTrue(resumed.live_ready(targets, 'ipv6'))
            original_get = kube.get
            def changed_address(kind, name):
                obj = original_get(kind, name)
                if kind == 'pod' and name == sources[0]['pod']:
                    obj['status']['podIPs'][1]['ip'] = '2001:db8::ffff'
                return obj
            with patch.object(kube, 'get', side_effect=changed_address), patch.object(resumed, 'progress') as progress:
                self.assertFalse(resumed.live_ready(targets, 'changed'))
                progress.assert_not_called()

    def test_timeout_intent_prevents_duplicate_mutation(self):
        with tempfile.TemporaryDirectory() as root:
            kube = Mock(); kube.exec_result.return_value = (b'', 124, 'inspect original')
            session = Session(kube, root); lane = {'source': {'nodeUID': 'uid'}}
            with self.assertRaises(RuntimeError): session.control(0, lane, 'start', ['probe'])
            with self.assertRaises(FileExistsError): session.control(0, lane, 'start', ['probe'])
            self.assertEqual(kube.exec_result.call_count, 1)
            self.assertEqual(json.loads((pathlib.Path(root)/'lane-00-start.response.json').read_text())['exitCode'], 124)

    def test_ready_requires_advancing_same_process(self):
        with tempfile.TemporaryDirectory() as root:
            p = pathlib.Path(root); session = Session(Mock(), p)
            (p/'session.json').write_text(json.dumps(dict(maxGap=10,lanes=[{}])))
            earlier = [dict(nonce='a', pid=3, attempts=10, observedAt='2026-09-08T00:00:00+00:00')]
            later = [dict(nonce='a', pid=3, attempts=11, observedAt='2026-09-08T00:00:01+00:00')]
            (p/'progress-a.json').write_text(json.dumps(earlier))
            for change in ({'attempts':10}, {'pid':4}, {'nonce':'b'}, {'observedAt':earlier[0]['observedAt']}):
                (p/'progress-b.json').write_text(json.dumps([dict(later[0], **change)]))
                with self.assertRaises(ValueError): session.ready('a','b')
            (p/'progress-b.json').write_text(json.dumps(later))
            from unittest.mock import patch
            with patch('survivor_native.fresh_progress', return_value=True):
                self.assertEqual(session.ready('a','b')['lanes'], 1)

    def test_native_loss_requires_explicit_diagnostic_session(self):
        f=json.loads((pathlib.Path(__file__).parent/'testdata/survivor-preflight-loss.json').read_text())
        for reason in [None, 'Observe GPU cache despite known baseline UDP loss']:
            with self.subTest(reason=reason),tempfile.TemporaryDirectory() as root:
                p=pathlib.Path(root);kube=Mock();session=Session(kube,p)
                (p/'session.json').write_text(json.dumps(dict(maxGap=f['maxGap'],lanes=[f['lane']],diagnosticReason=reason)))
                (p/'lane-00-receipt.json').write_text(json.dumps(f['receipt']))
                kube.exec_result.return_value=(json.dumps(f['status']).encode(),0,'')
                if reason is None:
                    with self.assertRaises(ValueError):session.progress('native')
                else:
                    row=session.progress('native')[0]
                    self.assertFalse(row['healthy'])
                    self.assertEqual(row['attempts']-row['passed'],1)

    def test_diagnostic_mode_still_rejects_broken_observation(self):
        import copy
        f=json.loads((pathlib.Path(__file__).parent/'testdata/survivor-preflight-loss.json').read_text())
        for changes in [{'nonce':'replacement'},{'pid':-1},{'attempts':0},
                        {'passed':-1},{'healthy':True},{'maxStartIntervalSeconds':11},
                        {'maxStartIntervalSeconds':float('nan')}]:
            with self.subTest(changes=changes),tempfile.TemporaryDirectory() as root:
                p=pathlib.Path(root);kube=Mock();session=Session(kube,p)
                (p/'session.json').write_text(json.dumps(dict(maxGap=f['maxGap'],lanes=[f['lane']],diagnosticReason='Diagnostic only')))
                (p/'lane-00-receipt.json').write_text(json.dumps(f['receipt']))
                status=copy.deepcopy(f['status']);status['latest'].update(changes)
                kube.exec_result.return_value=(json.dumps(status).encode(),0,'')
                with self.assertRaises(ValueError):session.progress('invalid')

    def test_diagnostic_completion_cannot_qualify_even_without_packet_loss(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root);session=Session(Mock(),p)
            (p/'READY.json').write_text(json.dumps(dict(progress=[dict(attempts=1)],diagnosticReason='Diagnostic only')))
            with patch.object(session,'_collect',return_value=([{'ok':True}],True)),patch.object(session,'assess_window',return_value={'ok':True}):
                report=session.finish('start','end',Mock())
            self.assertFalse(report['ok'])
            self.assertFalse(report['qualificationEligible'])
            self.assertEqual(report['diagnosticReason'],'Diagnostic only')

    def test_terminal_or_changed_process_never_ready(self):
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); kube=Mock(); session=Session(kube,p)
            (p/'session.json').write_text(json.dumps(dict(maxGap=10,lanes=[dict(source={}, executable='probe',directory='dir')])))
            (p/'lane-00-receipt.json').write_text(json.dumps(dict(nonce='original',pid=10)))
            for i,status in enumerate([dict(terminal=True),dict(terminal=False,latest=dict(nonce='other',pid=10)),dict(terminal=False,latest=dict(nonce='original',pid=11))]):
                kube.exec_result.return_value=(json.dumps(status).encode(),0,'')
                with self.assertRaises(ValueError): session.progress('round-'+str(i))

    def test_window_uses_host_control_order_and_original_nonce(self):
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            documents={
                'session.json':dict(lanes=[{}]),
                'READY.json':dict(readyAt='2026-09-08T00:00:01+00:00',lanes=1,progress=[dict(observedAt='2026-09-08T00:00:00+00:00',nonce='a',pid=3)]),
                'lane-00-receipt.json':dict(nonce='a',pid=3),
                'lane-00-stop.intent.json':dict(requestedAt='2026-09-08T00:00:04+00:00'),
                'lane-00-stop.response.json':dict(exitCode=0,stdout=json.dumps(dict(nonce='a')))}
            for name,value in documents.items(): (p/name).write_text(json.dumps(value))
            start,end='2026-09-08T00:00:02+00:00','2026-09-08T00:00:03+00:00'
            self.assertTrue(session.assess_window(start,end)['ok'])
            (p/'lane-00-stop.intent.json').write_text(json.dumps(dict(requestedAt=start)))
            self.assertFalse(session.assess_window(start,end)['ok'])
            with self.assertRaises(ValueError):session.assess_window(end,start)

    def test_finish_replays_native_failure_and_checks_all_logs(self):
        import gzip
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            for name,value in [('session.json',f['session']),('initial.json',f['initial']),('READY.json',dict(progress=f['progress']))]:
                (p/name).write_text(json.dumps(value))
            for i,lane in enumerate(f['lanes']):
                (p/f'lane-{i:02d}-receipt.json').write_text(json.dumps(lane['receipt']))
            def terminal(index,lane,operation,args): return f['lanes'][index]['terminal']
            def dump(source,args):
                index=next(i for i,l in enumerate(f['session']['lanes']) if l['directory']==args[2])
                return gzip.compress(f['lanes'][index]['raw'].encode())
            with patch.object(session,'stop') as stop,patch.object(session,'control',side_effect=terminal),patch.object(session,'assess_window',return_value={'ok':True}),patch('pod_matrix.snapshot',return_value=f['initial']):
                report=session.finish('start','end',dump)
            stop.assert_called_once()
            self.assertFalse(report['ok'])
            self.assertEqual(sum(r['attempts'] for r in report['lanes']),3604)
            self.assertEqual(sum(r['passed'] for r in report['lanes']),3603)
            self.assertTrue(report['identitiesStable'])
            self.assertEqual(len(list(p.glob('lane-*.jsonl.gz'))),6)

    def test_collection_parallelizes_hosts_but_preserves_lane_order(self):
        import gzip
        import threading
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        hosts={lane['source']['nodeUID'] for lane in f['session']['lanes']}
        self.assertGreater(len(hosts), 1)
        barrier=threading.Barrier(len(hosts), timeout=5)
        lock=threading.Lock(); active=set(); seen=set()
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            for name,value in [('session.json',f['session']),('initial.json',f['initial'])]:
                (p/name).write_text(json.dumps(value))
            for i,lane in enumerate(f['lanes']):
                (p/f'lane-{i:02d}-receipt.json').write_text(json.dumps(lane['receipt']))
            def dump(source,args):
                uid=source['nodeUID']
                with lock:
                    self.assertNotIn(uid, active, 'concurrent reads against one host')
                    active.add(uid); first=uid not in seen; seen.add(uid)
                try:
                    if first: barrier.wait()
                    index=next(i for i,l in enumerate(f['session']['lanes']) if l['directory']==args[2])
                    return gzip.compress(f['lanes'][index]['raw'].encode())
                finally:
                    with lock: active.remove(uid)
            with patch.object(session,'stop') as stop,patch.object(session,'control',side_effect=lambda i,*args:f['lanes'][i]['terminal']),patch('pod_matrix.snapshot',return_value=f['initial']):
                reports,stable=session._collect(dump,None)
            stop.assert_called_once()
            self.assertTrue(stable)
            self.assertEqual(seen,hosts)
            self.assertEqual([(r['source'],r['target']) for r in reports],
                [(l['source']['pod'],l['target']['pod']) for l in f['session']['lanes']])
            self.assertEqual(sum(r['attempts']-r['passed'] for r in reports),1)
            self.assertEqual(len(list(p.glob('lane-*.jsonl.gz'))),len(f['lanes']))

    def test_collection_failure_keeps_other_hosts_evidence_without_retry(self):
        import gzip
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        failed_host=f['session']['lanes'][0]['source']['nodeUID']
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            for name,value in [('session.json',f['session']),('initial.json',f['initial'])]:
                (p/name).write_text(json.dumps(value))
            for i,lane in enumerate(f['lanes']):
                (p/f'lane-{i:02d}-receipt.json').write_text(json.dumps(lane['receipt']))
            def dump(source,args):
                if source['nodeUID']==failed_host: raise RuntimeError('collection unavailable')
                index=next(i for i,l in enumerate(f['session']['lanes']) if l['directory']==args[2])
                return gzip.compress(f['lanes'][index]['raw'].encode())
            callback=Mock(side_effect=dump)
            with patch.object(session,'stop') as stop,patch.object(session,'control',side_effect=lambda i,*args:f['lanes'][i]['terminal']),patch('pod_matrix.snapshot') as snapshot:
                with self.assertRaisesRegex(RuntimeError,'collection unavailable'):
                    session._collect(callback,None)
            stop.assert_called_once(); snapshot.assert_not_called()
            self.assertEqual(sum(c.args[0]['nodeUID']==failed_host for c in callback.call_args_list),1)
            for i,lane in enumerate(f['session']['lanes']):
                if lane['source']['nodeUID']!=failed_host:
                    self.assertTrue((p/f'lane-{i:02d}.jsonl.gz').exists())
            self.assertFalse((p/'result.json').exists())

    def test_live_ready_rejects_missing_lane_without_contacting_vm(self):
        import gzip
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); kube=Mock();session=Session(kube,p)
            (p/'initial.json').write_text(json.dumps(f['initial']))
            f['session']['lanes'].pop()
            (p/'session.json').write_text(json.dumps(f['session']))
            targets=[s['pod']+'='+s['nodeUID'] for s in f['initial']]
            self.assertFalse(session.live_ready(targets,'check'))
            kube.exec_result.assert_not_called()

    def test_failed_preflight_collects_original_native_failure_without_ready(self):
        import gzip
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            for name,value in [('session.json',f['session']),('initial.json',f['initial'])]:
                (p/name).write_text(json.dumps(value))
            for i,lane in enumerate(f['lanes']):
                (p/f'lane-{i:02d}-receipt.json').write_text(json.dumps(lane['receipt']))
            def dump(source,args):
                index=next(i for i,l in enumerate(f['session']['lanes']) if l['directory']==args[2])
                return gzip.compress(f['lanes'][index]['raw'].encode())
            with patch.object(session,'stop') as stop,patch.object(session,'control',side_effect=lambda i,*args:f['lanes'][i]['terminal']),patch('pod_matrix.snapshot',return_value=f['initial']):
                report=session.finish_preflight_failure('UDP loss before claim submission',dump)
                with self.assertRaises(FileExistsError):
                    session.finish_preflight_failure('Cannot reset original failure',dump)
            stop.assert_called_once()
            self.assertFalse(report['ok'])
            self.assertFalse(report['window']['ok'])
            self.assertTrue(report['identitiesStable'])
            self.assertEqual(sum(r['attempts']-r['passed'] for r in report['lanes']),1)
            self.assertEqual(len(list(p.glob('lane-*.jsonl.gz'))),6)
            self.assertFalse((p/'READY.json').exists())

    def test_preflight_cleanup_cannot_replace_a_ready_operation(self):
        from unittest.mock import patch
        with tempfile.TemporaryDirectory() as root:
            p=pathlib.Path(root); session=Session(Mock(),p)
            (p/'READY.json').write_text('{}')
            with patch.object(session,'stop') as stop:
                with self.assertRaises(ValueError):session.finish_preflight_failure('wrong path',Mock())
            stop.assert_not_called()

    def test_live_ready_observes_progress_and_rechecks_identity(self):
        import copy
        import gzip
        from unittest.mock import patch
        f=json.loads(gzip.decompress((pathlib.Path(__file__).parent/'testdata/survivor-native-session-failure.json.gz').read_bytes()))
        for changed,advancing in [(False,True),(True,True),(False,False)]:
            with tempfile.TemporaryDirectory() as root:
                p=pathlib.Path(root);session=Session(Mock(),p)
                for name,value in [('initial.json',f['initial']),('session.json',f['session'])]:
                    (p/name).write_text(json.dumps(value))
                second=copy.deepcopy(f['initial'])
                if changed:second[0]['containerID']='containerd://'+'0'*64
                with patch('pod_matrix.snapshot',side_effect=[f['initial'],second]),patch('time.sleep'),patch('survivor_native.fresh_progress',return_value=True),patch.object(session,'progress',side_effect=[[dict(attempts=1)]*6,[dict(attempts=2 if advancing else 1)]*6]),patch.object(session,'ready') as ready:
                    result=session.live_ready([s['pod']+'='+s['nodeUID'] for s in f['initial']],'check')
                self.assertEqual(result,advancing and not changed)
                self.assertEqual(ready.call_count,int(result))
