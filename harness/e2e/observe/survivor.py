"""Sample UDP concurrently between existing ordinary survivor pods.

Start before a lifecycle operation; create OUTPUT/STOP after it finishes.
READY.json means every lane returned a successful probe. Failures are sticky.
Sampling gaps remain visible; this does not prove delivery between attempts.
"""
import argparse
import concurrent.futures
import datetime
import json
import math
import pathlib
import threading
import time
import urllib.parse

from pod_matrix import Kubectl, snapshot
from udp import evaluate


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def summarize(rows, max_gap):
    gaps = [b['startMonotonic'] - a['finishMonotonic'] for a, b in zip(rows, rows[1:])]
    starts = [b['startMonotonic'] - a['startMonotonic'] for a, b in zip(rows, rows[1:])]
    return dict(maxStartIntervalSeconds=max(starts, default=0), attempts=len(rows), passed=sum(r['ok'] for r in rows),
                maxGapSeconds=max(gaps, default=0),
                maxAttemptSeconds=max((r['finishMonotonic'] - r['startMonotonic'] for r in rows), default=0),
                ok=bool(rows) and all(r['ok'] for r in rows) and max(starts, default=0) <= max_gap)


def observe(kube, targets, output, duration=600, interval=0.1, max_gap=5):
    if (not all(math.isfinite(v) for v in (duration, interval, max_gap)) or
            duration <= 0 or interval < 0 or max_gap <= 0 or interval > max_gap):
        raise ValueError('invalid duration, interval or maximum sampling gap')
    output.mkdir(mode=0o700)
    initial = snapshot(kube, targets)
    (output/'initial.json').write_text(json.dumps(initial, indent=2)+'\n')
    started = time.monotonic()
    deadline = started + duration
    lock = threading.Lock()
    lanes = [(s, t) for s in initial for t in initial if s != t]
    healthy = set()

    def lane(index, source, target):
        rows = []
        failed = False
        with (output/f'lane-{index:02d}.jsonl').open('x') as stream:
            while time.monotonic() < deadline and not (output/'STOP').exists():
                begin = time.monotonic()
                stamp = utc()
                payload = f'{index:02d}:{len(rows):012d}'.ljust(64, 'x')
                query = urllib.parse.urlencode(dict(host=target['podIP'], port=8081,
                    protocol='udp', tries=1, request='echo '+payload))
                try:
                    body, code, diagnostic = kube.curl_result(source, ['http://127.0.0.1:8080/dial?'+query], timeout=10)
                    result = evaluate(body.decode('utf-8-sig', errors='replace'), payload, 1, code)
                    error = None
                except Exception as exc:
                    result, error = {'ok': False}, type(exc).__name__
                    diagnostic = 'Observer could not execute the probe'
                row = dict(result, sequence=len(rows), source=source['pod'], target=target['pod'],
                           startedAt=stamp, finishedAt=utc(), startMonotonic=begin,
                           finishMonotonic=time.monotonic(), error=error, executorDiagnostic=diagnostic)
                failed = failed or not row["ok"]
                rows.append(row)
                stream.write(json.dumps(row)+'\n'); stream.flush()
                with lock:
                    if not row['ok']:
                        healthy.discard(index)
                    elif not failed:
                        healthy.add(index)
                    if len(healthy) == len(lanes) and not (output/'READY.json').exists():
                        (output/'READY.json').write_text(json.dumps({'readyAt':utc(), 'lanes':len(lanes)})+'\n')
                time.sleep(interval)
        return dict(source=source['pod'], target=target['pod'], **summarize(rows, max_gap))

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(lanes)) as pool:
        futures = [pool.submit(lane, i, s, t) for i, (s, t) in enumerate(lanes)]
        reports = [f.result() for f in futures]
    try:
        stable = snapshot(kube, targets) == initial
        identity_error = None
    except Exception as exc:
        stable, identity_error = False, type(exc).__name__
    report = dict(finishedAt=utc(), elapsedSeconds=time.monotonic()-started,
        scope='Concurrent sampled 64-byte UDP echoes; excludes delivery between attempts and fragmented UDP qualification',
        executor=type(kube).__name__,
        maxAllowedGapSeconds=max_gap, intervalSeconds=interval,
        identitiesStable=stable, identityError=identity_error, lanes=reports,
        stopRequested=(output/'STOP').exists(), ready=(output/'READY.json').exists(),
        ok=stable and bool(reports) and all(r['ok'] for r in reports))
    (output/'result.json').write_text(json.dumps(report, indent=2)+'\n')
    return report


def assess_window(output, started_at, finished_at):
    """Require every directed lane to bracket the externally recorded operation.

    Readiness alone is insufficient: the original observer must finish, retain
    successful samples after completion, and pass its identity and gap checks.
    """
    start = datetime.datetime.fromisoformat(started_at)
    end = datetime.datetime.fromisoformat(finished_at)
    if start.tzinfo is None or end.tzinfo is None or start >= end:
        raise ValueError('ordered timezone-aware operation timestamps required')
    report = json.loads((output/'result.json').read_text())
    ready = json.loads((output/'READY.json').read_text())
    checks = []
    brackets = []
    for index, lane in enumerate(report['lanes']):
        rows = [json.loads(line) for line in (output/f'lane-{index:02d}.jsonl').read_text().splitlines()]
        brackets.append(bool(rows) and
            datetime.datetime.fromisoformat(rows[0]['finishedAt']) <= start and
            datetime.datetime.fromisoformat(rows[-1]['startedAt']) >= end)
        checks.append(bool(rows) and len(rows) == lane['attempts'] and
            all(r['sequence'] == i and r['source'] == lane['source'] and
                r['target'] == lane['target'] for i, r in enumerate(rows)) and
            summarize(rows, report['maxAllowedGapSeconds'])['ok'] and
            datetime.datetime.fromisoformat(rows[0]['finishedAt']) <= start and
            datetime.datetime.fromisoformat(rows[-1]['startedAt']) >= end)
    return dict(startedAt=started_at, finishedAt=finished_at, lanesBracketWindow=brackets, lanesPassed=checks,
        ok=bool(checks) and all(checks) and report['ok'] and
           datetime.datetime.fromisoformat(ready['readyAt']) <= start,
        scope='Successful sampled survivor probes bracket the operation; gaps and latency remain in the observer report')


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--api-server')
    p.add_argument('--bastion')
    p.add_argument('--namespace')
    p.add_argument('--target', action='append', metavar='POD=NODE_UID')
    p.add_argument('--output', type=pathlib.Path, required=True)
    p.add_argument('--duration', type=float, default=600)
    p.add_argument('--interval', type=float, default=0.1)
    p.add_argument('--max-gap', type=float, default=5)
    p.add_argument('--oob-executors', type=pathlib.Path, help='trusted Node UID to OS/CRI executor argv JSON')
    p.add_argument('--assess-window', type=pathlib.Path, help='existing JSON containing startedAt and finishedAt')
    p.add_argument('--window-output', type=pathlib.Path, help='new path for operation-window assessment')
    a=p.parse_args()
    if a.assess_window:
        if not a.window_output:p.error('--assess-window requires --window-output')
        window=json.loads(a.assess_window.read_text())
        report=assess_window(a.output,window['startedAt'],window['finishedAt'])
        with a.window_output.open('x') as stream:json.dump(report,stream,indent=2)
        print(json.dumps(report))
        return 0 if report['ok'] else 1
    if a.window_output:p.error('--window-output requires --assess-window')
    if not all((a.api_server,a.bastion,a.namespace,a.target)):
        p.error('observation requires --api-server, --bastion, --namespace and --target')
    if any('=' not in t for t in a.target):p.error('target requires POD=NODE_UID')
    kube=Kubectl(a.api_server,a.bastion,a.namespace)
    if a.oob_executors:
        from survivor_oob import OutOfBand
        kube=OutOfBand(a.api_server,a.bastion,a.namespace,json.loads(a.oob_executors.read_text()))
    report=observe(kube,
        [tuple(t.split('=',1)) for t in a.target],a.output,a.duration,a.interval,a.max_gap)
    print(json.dumps(report))
    return 0 if report['ok'] else 1

if __name__ == '__main__':
    raise SystemExit(main())
