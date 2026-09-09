"""Audit the fixed native experiment from its raw request and receiver logs."""
import argparse
import datetime
import hashlib
import json
import pathlib


def validate(work):
    read = lambda name: json.loads((work / name).read_text())
    timestamp = lambda raw: datetime.datetime.fromisoformat(raw.replace('Z', '+00:00'))
    result = read('result.json')
    if not result['ok'] or result['error'] is not None:
        raise ValueError('native experiment failed')
    egress = (work / 'run.json').exists() and read('run.json').get('egress', False)
    events = read('events.json')
    terminals = [e for e in events if e['event'] == 'terminal']
    starts = [e['label'] for e in events if e['event'] == 'spawn']
    expected = {prefix + '-' + gen + '-' + str(family)
                for prefix in ['server', 'warmup', 'reject-spoof', 'after-spoof']
                for gen in ['a', 'b'] for family in [4, 6]}
    expected |= {prefix + '-' + str(family)
                 for prefix in ['old-during-prepare', 'new-during-retire', 'retired-a']
                 for family in [4, 6]}
    if egress:
        expected |= {'server-egress-' + gen + '-' + str(f) for gen in ['a', 'b'] for f in [4, 6]}
        expected |= {'unbound-' + phase + '-' + str(f) for phase in ['switch-b', 'rollback-a', 'reswitch-b', 'retire-a'] for f in [4, 6]}
        for label, generation in [('egress-baseline', 'a'), ('switch-b', 'b'), ('rollback-a', 'a'), ('reswitch-b', 'b'), ('egress-after-retirement', 'b')]:
            for family in [4, 6]:
                routes = read(label + '-' + str(family) + '.json')
                if len(routes) != 1 or routes[0]['dev'] != 'cldt-hov-' + generation:
                    raise ValueError('native egress selection differs from requested generation')
    if set(starts) != expected:
        raise ValueError('incomplete experiment matrix')
    if len(starts) != len(set(starts)) or sorted(starts) != sorted(e['label'] for e in terminals):
        raise ValueError('missing, duplicate, or uncollected process')
    if any(e['exitCode'] != 0 for e in terminals):
        raise ValueError('native process did not succeed')
    cleanups = [e for e in events if e['event'] == 'cleanup']
    if sorted(e['node'] for e in cleanups) != ['cp2', 'w1', 'w2'] or any(e['exitCode'] != 0 for e in cleanups):
        raise ValueError('incomplete namespace cleanup')
    for node in ['cp2', 'w1', 'w2']:
        if len(read(node + '-hardware.json')['physicalNICs']) != 1:
            raise ValueError('experiment did not use single-NIC VMs')
    times = {e['event']: timestamp(e['at']) for e in events}
    audit, received = [], 0
    for label in starts:
        path = work / (label + '.jsonl')
        raw = path.read_bytes()
        if not raw.endswith(b'\n'):
            raise ValueError('incomplete raw log: ' + label)
        rows = [json.loads(line) for line in raw.splitlines()]
        if label.startswith('server-'):
            for row in rows:
                source = row['source'].rsplit(':', 1)[0].strip('[]')
                allowed = ['10.254.99.13', 'fd99:99::13'] if label.startswith('server-egress-') else ['10.254.99.11', 'fd99:99::11']
                if not row['replyOK'] or source not in allowed:
                    raise ValueError('receiver accepted unexpected traffic or failed to reply')
            received += len(rows)
            continue
        negative = label.startswith(('reject-', 'retired-'))
        count = 400 if ('during-' in label or label.startswith('unbound-')) else (2 if negative else (40 if label.startswith('warmup') else 20))
        if len(rows) != count:
            raise ValueError('missing requests: ' + label)
        previous = None
        for i, row in enumerate(rows):
            if row['sequence'] != i or not row['ok'] or row['expectedRejection'] != negative or row['echo'] == negative:
                raise ValueError('request failed or changed identity: ' + label)
            ipv6 = label.endswith('-6')
            spoof = label.startswith('reject-')
            expected_source = ('fd99:99::99' if spoof else 'fd99:99::11') if ipv6 else ('10.254.99.99' if spoof else '10.254.99.11')
            destination = '[fd99:99::13]:9001' if ipv6 else '10.254.99.13:9001'
            if label.startswith('unbound-'):
                expected_source = 'fd99:99::13' if ipv6 else '10.254.99.13'
                destination = '[fd99:99::11]:9002' if ipv6 else '10.254.99.11:9002'
            if row['source'].rsplit(':', 1)[0].strip('[]') != expected_source or row['destination'] != destination:
                raise ValueError('wrong source or destination: ' + label)
            if 'sendError' in row or (negative and 'i/o timeout' not in row.get('receiveError', '')) or (not negative and 'receiveError' in row):
                raise ValueError('unexpected transport failure: ' + label)
            start, end = timestamp(row['startedAt']), timestamp(row['finishedAt'])
            if end < start or (previous is not None and start < previous):
                raise ValueError('invalid request ordering: ' + label)
            previous = end
        phase = 'prepare-b' if label.startswith('old-during') else ('retire-a' if label.startswith('new-during') else None)
        if label.startswith('unbound-'):
            phase = label[len('unbound-'):-2]
        if phase and not (timestamp(rows[0]['startedAt']) < times[phase + '-start'] < times[phase + '-finished'] < timestamp(rows[-1]['finishedAt'])):
            raise ValueError('stream does not bracket operation: ' + label)
        gap = max((timestamp(rows[i]['startedAt']) - timestamp(rows[i - 1]['startedAt'])).total_seconds() for i in range(1, len(rows)))
        audit.append(dict(label=label, attempts=count, expectedRejection=negative,
                          continuous=phase is not None, maxStartIntervalSeconds=gap,
                          sha256=hashlib.sha256(raw).hexdigest()))
    positive = sum(r['attempts'] for r in audit if not r['expectedRejection'])
    if received != positive:
        raise ValueError('receiver packet count differs from successful requests')
    return dict(positiveRequests=positive, expectedRejections=sum(r['attempts'] for r in audit if r['expectedRejection']),
                continuousRequests=sum(r['attempts'] for r in audit if r['continuous']),
                streams=audit, ok=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('work', type=pathlib.Path)
    args = parser.parse_args()
    print(json.dumps(validate(args.work), indent=2))
