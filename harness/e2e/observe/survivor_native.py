"""Control pre-staged native UDP sampler binaries via exact-container VM OOB.

Staging is runner-owned; bulk collection uses a binary-safe callback. Mutating
commands are issued once and journaled before execution; timeouts require inspection.
"""
import datetime
import json
import pathlib


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def exclusive(path, value):
    with path.open('x') as stream:
        json.dump(value, stream, indent=2)
        stream.write('\n')


class Session:
    def __init__(self, kube, output):
        self.kube = kube
        self.output = pathlib.Path(output)

    def control(self, index, lane, operation, arguments):
        """Persist intent first; never replay start/stop on observation timeout."""
        path = self.output / f'lane-{index:02d}-{operation}'
        exclusive(path.with_suffix('.intent.json'), dict(requestedAt=utc(),
            source=lane['source'], arguments=arguments))
        body, code, diagnostic = self.kube.exec_result(lane['source'], arguments)
        exclusive(path.with_suffix('.response.json'), dict(observedAt=utc(),
            exitCode=code, stdout=body.decode('utf-8', errors='replace'), diagnostic=diagnostic))
        if code:
            raise RuntimeError(f'{operation} observation failed ({code}); inspect original operation before proceeding')
        return json.loads(body)

    def start(self, sources, executables, duration=3600, interval=0.1, max_gap=10,
              diagnostic_reason=None):
        if not 0 < duration <= 7200 or not 0.01 <= interval <= 1 or not interval <= max_gap <= 60:
            raise ValueError('invalid bounded stream configuration')
        if len(sources) < 2 or len({s['nodeUID'] for s in sources}) != len(sources):
            raise ValueError('distinct survivor Node identities required')
        if set(executables) != {s['nodeUID'] for s in sources}:
            raise ValueError('exact Node UID executable mapping required')
        if diagnostic_reason is not None and (not isinstance(diagnostic_reason, str) or not diagnostic_reason.strip()):
            raise ValueError('explicit diagnostic reason required')
        self.output.mkdir(mode=0o700)
        exclusive(self.output/'initial.json', sources)
        lanes = [dict(source=s, target=t, executable=executables[s['nodeUID']],
                      directory=executables[s['nodeUID']] + '-stream-' + str(i))
                 for i, (s, t) in enumerate((s, t) for s in sources for t in sources if s != t)]
        exclusive(self.output/'session.json', dict(lanes=lanes, maxGap=max_gap,
            duration=duration, interval=interval, transport='native-oob',
            diagnosticReason=diagnostic_reason))
        for i, lane in enumerate(lanes):
            address = lane['target']['podIP']
            if ':' in address: address = '[' + address + ']'
            receipt = self.control(i, lane, 'start', [lane['executable'],
                '-stream-dir', lane['directory'], '-destination', address+':8081',
                '-source-ip', lane['source']['podIP'], '-payload-bytes', '64',
                '-max-duration', f'{duration}s', '-interval', f'{interval}s', '-max-gap', f'{max_gap}s'])
            exclusive(self.output/f'lane-{i:02d}-receipt.json', receipt)
        return lanes

    def progress(self, label):
        """Read-only status; compare two rounds to prove native process progress."""
        import math
        if not label or any(c not in 'abcdefghijklmnopqrstuvwxyz0123456789-' for c in label):
            raise ValueError('simple unique progress label required')
        session = json.loads((self.output/'session.json').read_text())
        diagnostic = bool(session.get('diagnosticReason'))
        observations = []
        for i, lane in enumerate(session['lanes']):
            receipt = json.loads((self.output/f'lane-{i:02d}-receipt.json').read_text())
            status = self.control(i, lane, 'status-'+label,
                [lane['executable'], '-stream-dir', lane['directory'], '-status'])
            row = status.get('latest', {})
            attempts, passed = row.get('attempts'), row.get('passed')
            gap = row.get('maxStartIntervalSeconds', float('inf'))
            if (status.get('terminal') is not False or row.get('nonce') != receipt['nonce'] or
                    row.get('pid') != receipt['pid'] or
                    type(attempts) is not int or attempts < 1 or type(passed) is not int or
                    not 0 <= passed <= attempts or not isinstance(gap, (int, float)) or
                    not math.isfinite(gap) or gap < 0 or gap > session['maxGap'] or
                    type(row.get('healthy')) is not bool or row['healthy'] != (passed == attempts) or
                    (not diagnostic and not row['healthy'])):
                raise ValueError('original stream is not live and healthy')
            observations.append(dict(observedAt=utc(), attempts=attempts, passed=passed,
                healthy=row['healthy'], nonce=row['nonce'], pid=row['pid']))
        exclusive(self.output/f'progress-{label}.json', observations)
        return observations

    def ready(self, earlier, later):
        a = json.loads((self.output/f'progress-{earlier}.json').read_text())
        b = json.loads((self.output/f'progress-{later}.json').read_text())
        if len(a) != len(b) or not a or any(
                (x['nonce'], x['pid']) != (y['nonce'], y['pid']) or x['attempts'] >= y['attempts'] or
                datetime.datetime.fromisoformat(x['observedAt']) >= datetime.datetime.fromisoformat(y['observedAt'])
                for x, y in zip(a, b)):
            raise ValueError('two advancing progress rounds required')
        session = json.loads((self.output/'session.json').read_text())
        receipt = dict(readyAt=utc(), lanes=len(b), transport='native-oob', progress=b,
            diagnosticReason=session.get('diagnosticReason'))
        exclusive(self.output/'READY.json', receipt)
        return receipt

    def stop(self):
        session = json.loads((self.output/'session.json').read_text())
        for i, lane in enumerate(session['lanes']):
            receipt = json.loads((self.output/f'lane-{i:02d}-receipt.json').read_text())
            response = self.control(i, lane, 'stop',
                [lane['executable'], '-stream-dir', lane['directory'], '-stop'])
            if response.get('nonce') != receipt['nonce']:
                raise ValueError('stop acknowledged a different stream')
        exclusive(self.output/'STOP-ACK.json', dict(observedAt=utc()))

    def assess_window(self, started_at, finished_at):
        """Prove host control ordering without comparing clocks across VMs.

        Full raw-log validation and stable identity checks are additional gates.
        """
        start, end = map(datetime.datetime.fromisoformat, (started_at, finished_at))
        if start.tzinfo is None or end.tzinfo is None or start >= end:
            raise ValueError('ordered timezone-aware operation timestamps required')
        ready = json.loads((self.output/'READY.json').read_text())
        session = json.loads((self.output/'session.json').read_text())
        checks = []
        for i, lane in enumerate(session['lanes']):
            receipt = json.loads((self.output/f'lane-{i:02d}-receipt.json').read_text())
            intent = json.loads((self.output/f'lane-{i:02d}-stop.intent.json').read_text())
            response = json.loads((self.output/f'lane-{i:02d}-stop.response.json').read_text())
            progress = ready['progress'][i]
            checks.append(progress['nonce'] == receipt['nonce'] and progress['pid'] == receipt['pid'] and
                datetime.datetime.fromisoformat(progress['observedAt']) <= start and
                datetime.datetime.fromisoformat(intent['requestedAt']) >= end and
                response['exitCode'] == 0 and json.loads(response['stdout'])['nonce'] == receipt['nonce'])
        return dict(lanesBracketWindow=checks, ok=bool(checks) and all(checks) and
                    ready['lanes'] == len(checks) and datetime.datetime.fromisoformat(ready['readyAt']) <= start,
                    scope='Host OOB control ordering only; requires independent terminal logs and identity validation')

    def live_ready(self, targets, label):
        """Recheck exact Pod identities and actual progress before a mutation."""
        import time
        from pod_matrix import snapshot
        if (self.output/'result.json').exists() or list(self.output.glob('lane-*-stop.intent.json')):
            return False
        initial = json.loads((self.output/'initial.json').read_text())
        session = json.loads((self.output/'session.json').read_text())
        if (len(initial) < 2 or len({s['nodeUID'] for s in initial}) != len(initial) or
                {s['pod']+'='+s['nodeUID'] for s in initial} != set(targets)):
            return False
        expected = [(s,t) for s in initial for t in initial if s != t]
        if [(l['source'],l['target']) for l in session['lanes']] != expected:
            return False
        target_pairs = [tuple(t.split('=',1)) for t in targets]
        if snapshot(self.kube, target_pairs) != initial:
            return False
        earlier = self.progress(label+'-first')
        time.sleep(1)
        later = self.progress(label+'-second')
        if any(a['attempts'] >= b['attempts'] for a,b in zip(earlier,later)):
            return False
        if snapshot(self.kube, target_pairs) != initial:
            return False
        # Preserve the original pre-operation readiness for final window proof.
        if not (self.output/'READY.json').exists():
            self.ready(label+'-first',label+'-second')
        return True

    def finish(self, started_at, finished_at, dump):
        """Stop original processes once, collect terminal bytes and verify them.

        dump(source, argv) must return the complete binary-safe compressed log.
        A failed collection retains the original stop intents and guest files.
        """
        ready = json.loads((self.output/'READY.json').read_text())
        reports, stable = self._collect(dump, [p['attempts'] for p in ready['progress']])
        window=self.assess_window(started_at,finished_at)
        diagnostic = ready.get('diagnosticReason')
        report=dict(transport='native-oob',lanes=reports,identitiesStable=stable,
            window=window,diagnosticReason=diagnostic,qualificationEligible=not bool(diagnostic),
            ok=not diagnostic and stable and window['ok'] and all(r['ok'] for r in reports))
        exclusive(self.output/'result.json',report)
        return report

    def finish_preflight_failure(self, reason, dump):
        """Collect original samplers when readiness failed before an operation.

        The caller must establish that no lifecycle mutation was submitted.
        This preserves a failed result even if all collected packets succeeded.
        """
        if not isinstance(reason, str) or not reason.strip():
            raise ValueError('preflight failure reason required')
        if (self.output/'READY.json').exists():
            raise ValueError('ready sessions require their actual operation window')
        exclusive(self.output/'preflight-failure.json', dict(reason=reason, observedAt=utc()))
        reports, stable = self._collect(dump, None)
        report = dict(transport='native-oob', lanes=reports, identitiesStable=stable,
            window={'ok': False, 'scope': 'No qualified operation window'},
            preflightFailed=True, reason=reason, ok=False)
        exclusive(self.output/'result.json', report)
        return report

    def _collect(self, dump, progress_attempts):
        import gzip
        import time
        from pod_matrix import snapshot
        from survivor_stream import evaluate
        session = json.loads((self.output/'session.json').read_text())
        initial = json.loads((self.output/'initial.json').read_text())
        self.stop()
        reports=[]
        for i,lane in enumerate(session['lanes']):
            for attempt in range(30):
                status=self.control(i,lane,'terminal-'+str(attempt),
                    [lane['executable'],'-stream-dir',lane['directory'],'-status'])
                if status.get('terminal') is True: break
                time.sleep(1)
            else:
                raise RuntimeError('original stream has not acknowledged stop; inspect it')
            compressed=dump(lane['source'],[lane['executable'],'-stream-dir',lane['directory'],'-dump'])
            with (self.output/f'lane-{i:02d}.jsonl.gz').open('xb') as stream: stream.write(compressed)
            receipt=json.loads((self.output/f'lane-{i:02d}-receipt.json').read_text())
            result=evaluate(gzip.decompress(compressed),receipt,status['result'],
                lane['source']['podIP'],session['maxGap'],
                progress_attempts[i] if progress_attempts is not None else None)
            reports.append(dict(source=lane['source']['pod'],target=lane['target']['pod'],**result))
        stable=snapshot(self.kube,[(s['pod'],s['nodeUID']) for s in initial]) == initial
        return reports, stable
