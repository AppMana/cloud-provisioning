#!/usr/bin/env python3
"""Validate collected native evidence without starting or stopping any process."""
import argparse
import datetime
import hashlib
import ipaddress
import json
import pathlib

PHASES = ['baseline', 'switch-b', 'rollback-a', 'reselect-b', 'retire-a']


def timestamp(raw):
    return datetime.datetime.fromisoformat(raw.replace('Z', '+00:00')).timestamp()


def endpoint_ip(raw):
    host, port = raw.rsplit(':', 1)
    assert 0 < int(port) < 65536
    return str(ipaddress.ip_address(host.strip('[]')))


def validate(root, year, require_isolation=False):
    def read(name):
        return (root / f'raw-{year}-{name}').read_text()
    summary = json.loads(read('summary.json'))
    ready = json.loads(read('ready.json'))
    process = json.loads(read('process.json'))
    assert summary['run'] == ready['run'] and ready['pid'] == process['pid']
    rows = [json.loads(line) for line in read('events.jsonl').splitlines()]
    assert int(read('exit-code.txt')) == 0, 'native test failed'
    assert summary['phases'] == PHASES
    packets = [r for r in rows if r['kind'] == 'packet' and r['phase'] != 'warmup']
    assert len(packets) == summary['packets'] and len(packets) >= 500
    assert [r['sequence'] for r in packets] == list(range(len(packets)))
    assert summary['failures'] == 0 and not any(r['error'] for r in packets)
    assert {r['phase'] for r in packets} == set(PHASES)
    assert len({r['source'] for r in packets}) == 1, 'socket/source identity changed'
    assert len({r['destination'] for r in packets}) == 1
    assert all(r['bytes'] == 64 for r in packets)
    times = [timestamp(r['at']) for r in packets]
    assert all(b >= a for a, b in zip(times, times[1:]))
    assert times[0] >= timestamp(summary['start'])
    assert times[-1] - times[0] >= 49
    assert all(timestamp(r['end']) >= timestamp(r['at']) for r in packets)
    routes = [r for r in rows if r['kind'] == 'route']
    assert [r['stage'] for r in routes] == ['prepared'] + PHASES[1:]
    assert [r['selected'] for r in routes] == [0, 1, 0, 1, 1]
    assert len({r['ownerLUID'] for r in routes}) == 1
    assert len({r['source'] for r in routes}) == 1
    assert all(r['source'] == r['bestSource'] and r['luid'] != r['ownerLUID'] for r in routes)
    assert routes[0]['luid'] == routes[2]['luid']
    assert routes[1]['luid'] == routes[3]['luid'] == routes[4]['luid']
    starts = [r for r in rows if r['kind'] == 'phase-start']
    assert [r['stage'] for r in starts] == PHASES, 'independent-sampler phase evidence missing'
    for start, route in zip(starts[1:], routes[1:]):
        assert timestamp(start['at']) <= timestamp(route['at'])
    assert 'all owned stable-owner adapters removed' in read('stdout.txt')
    cleanup = json.loads((root / f'cleanup-{year}-result.json').read_text())
    assert cleanup['Status'] == 'Success' and cleanup['ResponseCode'] == 0
    assert 'owned adapters absent; process absent' in cleanup['StandardOutputContent']
    probes = [r for r in rows if r['kind'] == 'source-probe']
    isolation = None
    if require_isolation or probes:
        controls = [r for r in probes if not r['deny']]
        negatives = [r for r in probes if r['deny']]
        assert any(r['passed'] and r['writeSucceeded'] and not r['readTimedOut'] for r in controls), 'positive source control missing'
        assert {r.get('generation') for r in controls if r['passed']} == {0, 1}, 'positive source controls must cover both generations'
        warm_routes = [r for r in rows if r['kind'] == 'warmup-route']
        assert warm_routes and {r['selected'] for r in warm_routes} == {0, 1} and warm_routes[-1]['selected'] == 0
        for control in controls:
            if not control['passed']:
                continue
            preceding = [r for r in warm_routes if timestamp(r['at']) <= timestamp(control['at'])]
            assert preceding and preceding[-1]['selected'] == control['generation']
            assert preceding[-1]['source'] == endpoint_ip(control['source']) == preceding[-1]['bestSource']
            assert preceding[-1]['luid'] == routes[control['generation']]['luid']

        assert len(negatives) == 10
        assert [r['sequence'] for r in negatives] == list(range(10))
        assert [r.get('generation') for r in negatives] == [0,0,1,1,0,0,1,1,1,1]
        source_routes = [r for r in rows if r['kind'] == 'source-route']
        assert [r['stage'] for r in source_routes] == ['source-'+phase for phase in PHASES]
        assert [r['selected'] for r in source_routes] == [0,1,0,1,1]
        assert all(r['source'] == r['bestSource'] == endpoint_ip(negatives[0]['source']) for r in source_routes)
        assert all(r['luid'] == routes[r['selected']]['luid'] for r in source_routes)

        assert all(r['passed'] and r['writeSucceeded'] and r['readTimedOut'] for r in negatives)
        assert all(timestamp(r['end']) - timestamp(r['at']) >= 0.24 for r in negatives)
        assert [r['phase'] for r in negatives] == [phase for phase in PHASES for _ in range(2)]
        assert len({r['source'] for r in probes}) == 1, 'alternate source socket changed'
        assert endpoint_ip(probes[0]['source']) != endpoint_ip(packets[0]['source'])
        assert any(r['kind'] == 'source-control-received' for r in rows), 'receiver-side positive control missing'
        assert not any(r['kind'] == 'forbidden-source' for r in rows), 'forbidden source reached application'
        armed = [r for r in rows if r['kind'] == 'isolation-armed']
        assert [r['generation'] for r in armed] == [0, 1]
        assert all(r['allowedSource'] != r['excludedSource'] for r in armed)
        assert len({r['allowedSource'] for r in armed}) == 1 and len({r['excludedSource'] for r in armed}) == 1
        assert armed[0]['allowedSource'] == endpoint_ip(packets[0]['destination'])
        received = [r for r in rows if r['kind'] == 'source-control-received']
        assert all(endpoint_ip(r['source']) == armed[0]['excludedSource'] for r in received)
        other_year = '2025' if year == '2022' else '2022'
        other_rows = [json.loads(line) for line in (root / f'raw-{other_year}-events.jsonl').read_text().splitlines()]
        other_armed = [r for r in other_rows if r['kind'] == 'isolation-armed']
        assert [r['generation'] for r in other_armed] == [0, 1]
        assert all(r['allowedSource'] == endpoint_ip(packets[0]['source']) and r['excludedSource'] == endpoint_ip(probes[0]['source']) for r in other_armed), 'peer source permissions do not match actual senders'
        assert max(timestamp(r['end']) for r in controls) < min(timestamp(r['at']) for r in armed), 'positive controls overlap isolation measurement'

        assert max(timestamp(r['at']) for r in armed) < min(timestamp(r['at']) for r in negatives)
        for negative in negatives:
            assert any(p['phase'] == negative['phase'] and timestamp(p['at']) >= timestamp(negative['at']) and timestamp(p['at']) <= timestamp(negative['end']) for p in packets), 'no authorized traffic during source rejection'
        isolation = {'rejectedProbes': len(negatives), 'successfulControlProbes': sum(r['passed'] for r in controls), 'successfulControlsByGeneration': {str(g): sum(r['passed'] and r['generation'] == g for r in controls) for g in [0,1]}, 'forbiddenPacketsReceived': 0}
    return {'packets': len(packets), 'failed': 0,
            'phaseCounts': {p: sum(r['phase'] == p for r in packets) for p in PHASES},
            'maxStartGapSeconds': max(b-a for a, b in zip(times, times[1:])),
            'source': packets[0]['source'], 'destination': packets[0]['destination'],
            'eventsSHA256': hashlib.sha256(read('events.jsonl').encode()).hexdigest(), 'sourceIsolation': isolation}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('work_dir', type=pathlib.Path)
    parser.add_argument('--require-isolation', action='store_true')
    args = parser.parse_args()
    print(json.dumps({year: validate(args.work_dir, year, args.require_isolation) for year in ['2022', '2025']}, indent=2))
