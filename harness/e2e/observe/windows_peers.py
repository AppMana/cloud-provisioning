"""Summarize native tunnelobserve JSONL without treating a missed transition as proof."""
import argparse
import datetime
import hashlib
import json
import pathlib
import re


def evaluate(rows, require_addition=False, require_removal=False, expected_peer=None):
    if expected_peer is not None and not re.fullmatch(r"[a-f0-9]{64}", expected_peer):
        raise ValueError("Expected peer must be a lowercase SHA256")
    if len(rows) < 2:
        raise ValueError('Require at least two native samples')
    maps = []
    previous_time = None
    for row in rows:
        at = datetime.datetime.fromisoformat(row['observedAt'].replace('Z', '+00:00'))
        if at.tzinfo is None or (previous_time is not None and at <= previous_time):
            raise ValueError('Sample timestamps must increase and include a timezone')
        previous_time = at
        if row['interface'] != rows[0]['interface']:
            raise ValueError('Adapter identity changed within capture')
        peers = {peer['publicKeySHA256']: peer for peer in row['peers']}
        if len(peers) != len(row['peers']):
            raise ValueError('Duplicate peer identity')
        for peer in peers.values():
            for field in ['txBytes', 'rxBytes']:
                if type(peer[field]) is not int or peer[field] < 0:
                    raise ValueError('Invalid native byte counter')
        maps.append(peers)
    changes = []
    resets = []
    for index in range(1, len(rows)):
        old, new = maps[index-1], maps[index]
        if old.keys() != new.keys():
            changes.append({'observedAt': rows[index]['observedAt'],
                'added': sorted(new.keys()-old.keys()), 'removed': sorted(old.keys()-new.keys())})
        for identity in sorted(old.keys() & new.keys()):
            if any(new[identity][field] < old[identity][field] for field in ['txBytes', 'rxBytes']):
                resets.append({'observedAt': rows[index]['observedAt'], 'identity': identity,
                    'before': {field: old[identity][field] for field in ['txBytes', 'rxBytes']},
                    'after': {field: new[identity][field] for field in ['txBytes', 'rxBytes']}})
    survivors = set.intersection(*(set(peers) for peers in maps))
    checks = {'allSamplesAdapterUp': all(row['up'] is True for row in rows),
              'noObservedCounterDecreases': not resets,
              'survivingPeersPresent': bool(survivors)}
    if require_addition:
        checks['additionObserved'] = any(expected_peer in change['added'] if expected_peer else bool(change['added']) for change in changes)
    if require_removal:
        checks['removalObserved'] = any(expected_peer in change['removed'] if expected_peer else bool(change['removed']) for change in changes)
    return {'passed': all(checks.values()), 'checks': checks, 'samples': len(rows),
            'firstObservedAt': rows[0]['observedAt'], 'lastObservedAt': rows[-1]['observedAt'],
            'peerCounts': sorted(set(len(peers) for peers in maps)),
            'membershipChanges': changes, 'counterDecreases': resets,
            'survivorCount': len(survivors), 'expectedPeerSHA256': expected_peer,
            'scope': 'Sampled peer membership and byte counters; does not establish packet delivery, between-sample continuity, or the cause of changes'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--input', required=True, type=pathlib.Path)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--require-addition', action='store_true')
    parser.add_argument('--require-removal', action='store_true')
    parser.add_argument('--expected-peer-sha256', help='Hash of the target Machine public key, captured from the peer Secret')
    args = parser.parse_args()
    raw = args.input.read_bytes()
    result = evaluate([json.loads(line) for line in raw.splitlines()], args.require_addition, args.require_removal, args.expected_peer_sha256)
    result['rawSHA256'] = hashlib.sha256(raw).hexdigest()
    with args.output.open('x') as output:
        json.dump(result, output, indent=2);output.write('\n')
    print(json.dumps({key: result[key] for key in ['passed', 'checks', 'samples']}))
    if not result['passed']:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
