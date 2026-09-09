import datetime
import json
import pathlib
import tempfile
import unittest
from unittest.mock import patch
import subprocess
from pod_matrix import Kubectl

from survivor import observe, summarize, assess_window
from test_pod_matrix import Cluster as MatrixCluster


class Cluster(MatrixCluster):
    def curl_result(self, *args, **kwargs):
        body, code = self.curl(*args, **kwargs)
        return body, code, "fixture executor failure" if code else ""



class SurvivorTest(unittest.TestCase):
    def test_native_linux_windows_samples_and_injected_regressions(self):
        fixture=json.loads((pathlib.Path(__file__).parent/'testdata/survivor-native-baseline.json').read_text())
        self.assertEqual(len(fixture['lanes']),6)
        for rows in fixture['lanes']:
            self.assertTrue(summarize(rows,5)['ok'])
            rows[2]['ok']=False
            self.assertFalse(summarize(rows,5)['ok'])
            rows[2]['ok']=True
            rows[-1]['startMonotonic']+=10
            rows[-1]['finishMonotonic']+=10
            self.assertFalse(summarize(rows,5)['ok'])

    def test_executor_diagnostics_are_retained_without_changing_matrix_api(self):
        kube=Kubectl('https://test','test','test')
        source=dict(pod='test',container='serve',os='linux')
        result=subprocess.CompletedProcess([],1,stdout=b'{}',stderr=b'No agent available')
        with patch('pod_matrix.subprocess.run',return_value=result):
            self.assertEqual(kube.curl_result(source,[]),(b'{}',1,'No agent available'))
            self.assertEqual(kube.curl(source,[]),(b'{}',1))
        with patch('pod_matrix.subprocess.run',side_effect=subprocess.TimeoutExpired('test',30)):
            self.assertEqual(kube.curl_result(source,[])[1],124)

    def test_native_exact_echo_with_executor_failure_still_fails(self):
        fixture=json.loads((pathlib.Path(__file__).parent/'testdata/survivor-cp2-reboot-failures.json').read_text())
        matching=[r for r in fixture['failures'] if r['exactResponses']==1]
        self.assertTrue(matching)
        for row in matching:
            self.assertNotEqual(row['commandExitCode'],0)
            self.assertFalse(summarize([row],5)['ok'])

    def test_native_oob_samples_and_transport_failure(self):
        fixture=json.loads((pathlib.Path(__file__).parent/'testdata/survivor-oob-native.json').read_text())
        self.assertEqual(len(fixture['lanes']),6)
        for rows in fixture['lanes']:
            self.assertTrue(summarize(rows,30)['ok'])
            rows[-1]['ok']=False
            self.assertFalse(summarize(rows,30)['ok'])

    def test_failure_is_sticky_and_gaps_are_not_hidden(self):
        rows = [dict(ok=True,startMonotonic=0,finishMonotonic=1),
                dict(ok=False,startMonotonic=2,finishMonotonic=3),
                dict(ok=True,startMonotonic=10,finishMonotonic=11)]
        self.assertFalse(summarize(rows, 20)['ok'])
        rows[1]['ok'] = True
        self.assertFalse(summarize(rows, 5)['ok'])
        self.assertEqual(summarize(rows, 20)['maxGapSeconds'], 7)
        self.assertFalse(summarize([], 20)['ok'])

    def test_live_container_change_cannot_pass_good_echoes(self):
        with tempfile.TemporaryDirectory() as root:
            report = observe(Cluster('restart'),Cluster.targets,pathlib.Path(root)/'run',.05,0,5)
            self.assertFalse(report['identitiesStable'])
            self.assertFalse(report['ok'])
            self.assertTrue(all(r['passed'] == r['attempts'] for r in report['lanes']))

    def test_executor_exception_is_retained_and_other_lanes_finish(self):
        class Broken(Cluster):
            def curl(self, *args, **kwargs): raise TimeoutError()
        with tempfile.TemporaryDirectory() as root:
            out=pathlib.Path(root)/'run'
            report=observe(Broken(),Cluster.targets,out,.05,0,5)
            self.assertFalse(report['ok'])
            self.assertFalse(report['ready'])
            self.assertEqual(len(list(out.glob('lane-*.jsonl'))),6)
            self.assertIn('TimeoutError',(out/'lane-00.jsonl').read_text())

    def test_existing_evidence_and_invalid_timing_rejected(self):
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaises(FileExistsError):
                observe(None,[],pathlib.Path(root))
            for duration in [0,-1,float('inf'),float('nan')]:
                with self.assertRaises(ValueError):
                    observe(None,[],pathlib.Path(root)/'unused',duration)

    def test_window_must_have_samples_before_and_after_operation(self):
        with tempfile.TemporaryDirectory() as root:
            out=pathlib.Path(root)
            def stamp(n): return f'2026-09-08T00:00:{n:02d}+00:00'
            rows=[dict(sequence=i,source='a',target='b',ok=True,startMonotonic=i*2,
                finishMonotonic=i*2+1,startedAt=stamp(i*2),finishedAt=stamp(i*2+1)) for i in range(4)]
            (out/'lane-00.jsonl').write_text(''.join(json.dumps(r)+'\n' for r in rows))
            (out/'READY.json').write_text(json.dumps(dict(readyAt=stamp(1))))
            (out/'result.json').write_text(json.dumps(dict(ok=True,maxAllowedGapSeconds=5,
                lanes=[dict(source='a',target='b',attempts=4)])))
            self.assertTrue(assess_window(out,stamp(2),stamp(5))['ok'])
            self.assertFalse(assess_window(out,stamp(2),stamp(8))['ok'])
            self.assertFalse(assess_window(out,stamp(0),stamp(5))['ok'])
