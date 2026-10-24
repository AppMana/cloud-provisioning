"""Continuously sample each explicit API and its exec/log path to bound pods.

READY.json requires two successful samples per lane. STOP requests one final
sample per lane; deadline expiry is incomplete. Failures and sampling gaps are
sticky. This proves sampled availability, not delivery between samples.
Failed responses retain bounded diagnostic text in the private sample files;
diagnostics never change the availability verdict or trigger a retry.
"""
import argparse
import concurrent.futures
import datetime
import hashlib
import json
import math
import os
import pathlib
import secrets
import subprocess
import threading
import time

from pod_matrix import Kubectl, snapshot
from survivor import summarize, utc


def command(server, namespace, pod, operation):
    args = ['kubectl', '--server=' + server, '--request-timeout=5s']
    if operation == 'api':
        return args + ['get', '--raw=/readyz']
    args += ['-n', namespace, operation, pod]
    if operation == 'logs':
        return args + ['--tail=1']
    if operation != 'exec':
        raise ValueError('unknown management operation')
    return args + ['--', 'wget', '-q', '-T', '3', '-O', '-',
                   'http://127.0.0.1:8080/echo?msg=ok']


def successful(operation, body, code):
    return code == 0 and (operation == 'logs' or body.strip() == b'ok')


def complete(lanes, stable, stop, ready):
    return bool(lanes) and stable and stop and ready and all(x['ok'] for x in lanes)


def response_evidence(code, stdout, stderr):
    evidence = {'returnCode': code}
    for name, value in (('stdout', stdout), ('stderr', stderr)):
        value = value or b''
        evidence[name] = value[:4096].decode('utf-8', errors='replace')
        evidence[name + 'Bytes'] = len(value)
    return evidence


def observe(kube, servers, targets, output, duration=600, interval=1, max_gap=10):
    if (not servers or len(set(servers)) != len(servers) or
            not all(s.startswith('https://') for s in servers) or
            not all(math.isfinite(v) for v in (duration, interval, max_gap)) or
            not 0 < interval <= max_gap or not 0 < duration <= 90000):
        raise ValueError('explicit distinct HTTPS APIs and bounded sampling parameters required')
    output.mkdir(mode=0o700)
    initial = snapshot(kube, targets)
    nonce, pid = secrets.token_hex(16), os.getpid()
    lanes = [dict(server=s, pod='', operation='api') for s in servers]
    lanes += [dict(server=s, pod=p['pod'], operation=op)
              for s in servers for p in initial for op in ('exec', 'logs')]
    receipt = dict(nonce=nonce, pid=pid, startedAt=utc(), targets=initial,
                   lanes=lanes, intervalSeconds=interval, maxGapSeconds=max_gap)
    (output / 'initial.json').write_text(json.dumps(receipt, indent=2) + '\n')
    executor_env = dict(os.environ, GOMAXPROCS='2')
    start = time.monotonic()
    lock, ready_lanes = threading.Lock(), set()

    def sample(index, lane):
        rows, failed, stopped = [], False, False
        with (output / f'lane-{index:02d}.jsonl').open('x') as stream:
            while time.monotonic() - start < duration:
                stopped = (output / 'STOP').exists()
                begin, stamp = time.monotonic(), utc()
                # Each lane is already concurrent; bound per-process Go scheduling
                # overhead without reducing requests or changing timeouts.
                args = ['docker', 'exec', '-e', 'GOMAXPROCS=2', kube.bastion] + command(
                    lane['server'], kube.namespace, lane['pod'], lane['operation'])
                # The bastion and namespace are explicit Kubectl configuration;
                # never use a host kubeconfig or an API failover helper.
                diagnostic = None
                try:
                    result = subprocess.run(args, capture_output=True, timeout=8, env=executor_env)
                    ok = successful(lane['operation'], result.stdout, result.returncode)
                    error = None if ok else 'management response failed'
                    if not ok:
                        diagnostic = response_evidence(result.returncode, result.stdout, result.stderr)
                except subprocess.TimeoutExpired as exc:
                    ok, error = False, 'management observation timeout'
                    diagnostic = response_evidence(None, exc.stdout, exc.stderr)
                except OSError:
                    ok, error = False, 'management executor unavailable'
                row = dict(nonce=nonce, pid=pid, sequence=len(rows), startedAt=stamp,
                           finishedAt=utc(), startMonotonic=begin, finishMonotonic=time.monotonic(),
                           ok=ok, error=error, stopBoundary=stopped)
                if diagnostic is not None:
                    row['response'] = diagnostic
                gap = begin - rows[-1]['startMonotonic'] if rows else 0
                rows.append(row)
                stream.write(json.dumps(row) + '\n'); stream.flush()
                failed = failed or not ok or gap > max_gap
                with lock:
                    if failed:
                        ready_lanes.discard(index)
                    elif len(rows) >= 2:
                        ready_lanes.add(index)
                    if len(ready_lanes) == len(lanes) and not (output / 'READY.json').exists():
                        (output / 'READY.json').write_text(json.dumps(dict(
                            nonce=nonce, pid=pid, readyAt=utc(), lanes=len(lanes))) + '\n')
                if stopped:
                    break
                time.sleep(interval)
        return dict(lane, **summarize(rows, max_gap), stopAcknowledged=stopped,
                    sha256=hashlib.sha256((output / f'lane-{index:02d}.jsonl').read_bytes()).hexdigest())

    with concurrent.futures.ThreadPoolExecutor(max_workers=len(lanes)) as pool:
        reports = list(pool.map(lambda entry: sample(*entry), enumerate(lanes)))
    try:
        stable = snapshot(kube, targets) == initial
    except Exception:
        stable = False
    stopped = all(r['stopAcknowledged'] for r in reports)
    ready = (output / 'READY.json').exists()
    report = dict(nonce=nonce, pid=pid, finishedAt=utc(), lanes=reports,
                  identitiesStable=stable, stopAcknowledged=stopped, ready=ready,
                  ok=complete(reports, stable, stopped, ready))
    (output / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
    return report


def assess_window(output, started_at, finished_at):
    """Independently verify retained samples bracket the requested operation."""
    start, end = map(datetime.datetime.fromisoformat, (started_at, finished_at))
    if start.tzinfo is None or end.tzinfo is None or start >= end:
        raise ValueError('ordered timezone-aware operation times required')
    receipt = json.loads((output / 'initial.json').read_text())
    result = json.loads((output / 'result.json').read_text())
    ready = json.loads((output / 'READY.json').read_text())
    identity = (receipt['nonce'], receipt['pid'])
    if any((r['nonce'], r['pid']) != identity for r in (result, ready)):
        raise ValueError('observer identity changed')
    if len(result['lanes']) != len(receipt['lanes']) or not result['lanes']:
        raise ValueError('management lanes changed or missing')
    if datetime.datetime.fromisoformat(ready['readyAt']) > start:
        raise ValueError('operation preceded observer readiness')
    for i, (lane, expected) in enumerate(zip(result['lanes'], receipt['lanes'])):
        if any(lane[k] != v for k, v in expected.items()):
            raise ValueError('management lane identity changed')
        raw = (output / f'lane-{i:02d}.jsonl').read_bytes()
        if not raw.endswith(b'\n') or hashlib.sha256(raw).hexdigest() != lane['sha256']:
            raise ValueError('sample evidence changed or incomplete')
        rows = [json.loads(x) for x in raw.splitlines()]
        for j, row in enumerate(rows):
            if ((row['nonce'], row['pid']) != identity or row['sequence'] != j or
                    type(row['ok']) is not bool or row['stopBoundary'] != (j == len(rows)-1) or
                    not all(math.isfinite(row[k]) for k in ('startMonotonic', 'finishMonotonic')) or
                    row['finishMonotonic'] < row['startMonotonic'] or
                    (j and row['startMonotonic'] < rows[j-1]['finishMonotonic'])):
                raise ValueError('invalid sample identity, order or stop boundary')
        measured = summarize(rows, receipt['maxGapSeconds'])
        if len(rows) < 3 or any(lane[k] != v for k, v in measured.items()) or not measured['ok']:
            raise ValueError('failed samples or mismatched summary')
        if (datetime.datetime.fromisoformat(rows[1]['finishedAt']) > start or
                datetime.datetime.fromisoformat(rows[-1]['startedAt']) < end or not lane['stopAcknowledged']):
            raise ValueError('samples do not bracket the operation')
    if not complete(result['lanes'], result['identitiesStable'], result['stopAcknowledged'], result['ready']) or not result['ok']:
        raise ValueError('management observer did not finish successfully')
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--server', action='append', required=True)
    p.add_argument('--bastion', required=True)
    p.add_argument('--namespace', required=True)
    p.add_argument('--target', action='append', required=True, help='pod=NodeUID')
    p.add_argument('--service', action='append', default=[], help='pod=existing-Service-name')
    p.add_argument('--output', type=pathlib.Path, required=True)
    p.add_argument('--duration', type=float, default=600)
    p.add_argument('--interval', type=float, default=1)
    p.add_argument('--max-gap', type=float, default=10)
    a = p.parse_args()
    targets = [tuple(t.split('=', 1)) for t in a.target]
    if any(len(t) != 2 or not all(t) for t in targets):
        p.error('each target requires pod=NodeUID')
    kube = Kubectl(a.server[0], a.bastion, a.namespace)
    for value in a.service:
        pod, separator, service = value.partition('=')
        if not separator or not service or pod not in {t[0] for t in targets} or pod in kube.service_names:
            p.error('each service mapping requires a distinct target pod=Service-name')
        kube.service_names[pod] = service
    result = observe(kube, a.server, targets, a.output, a.duration, a.interval, a.max_gap)
    print(json.dumps(result))
    raise SystemExit(0 if result['ok'] else 1)


if __name__ == '__main__':
    main()
