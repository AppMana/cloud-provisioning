"""Validate observed worker-first withdrawal without accepting an old ACK.

The input is an identity-bound API sampler's public hashes, not Secret contents.
Sampling cannot prove uninterrupted network traffic; retain native probes and
the independent CAPI/EC2 cleanup receipt for that purpose.
"""
import datetime
import re


def evaluate(rows, secret_uid):
    if not rows or not secret_uid:
        raise ValueError('original samples and worker Secret UID required')
    times = [datetime.datetime.fromisoformat(r['at'].replace('Z', '+00:00')) for r in rows]
    if any(t.tzinfo is None for t in times) or any(a >= b for a, b in zip(times, times[1:])):
        raise ValueError('sample timestamps must be ordered and timezone-aware')
    staged = [i for i, r in enumerate(rows) if r['projectionPresent'] and r['retiringWorker']]
    withdrawn = [i for i, r in enumerate(rows) if r['machineDeleting'] and not r['projectionPresent']]
    first = withdrawn[0] if withdrawn else len(rows)
    target = rows[first]['workerDocumentSHA256'] if withdrawn else None
    valid_target = bool(target and re.fullmatch('[a-f0-9]{64}', target))
    before_stage = rows[:staged[0]] if staged else []
    original = before_stage[-1]['workerDocumentSHA256'] if before_stage else None
    pending = [i for i in staged if i < first and valid_target and
               rows[i]['workerDocumentSHA256'] == target and rows[i]['workerAppliedSHA256'] != target]
    acked = [i for i in staged if i < first and valid_target and
             rows[i]['workerDocumentSHA256'] == target and rows[i]['workerAppliedSHA256'] == target]
    # Each row reads the mesh before the ACK. The following row's unchanged
    # resourceVersion brackets that ACK with the same committed projection.
    stable_acks = [i for i in acked if i + 1 < first and i + 1 in staged and
                   rows[i]['meshResourceVersion'] == rows[i + 1]['meshResourceVersion']]
    checks = {
        'sameMesh': bool(rows[0]['meshUID']) and all(r['meshUID'] == rows[0]['meshUID'] for r in rows),
        'sameWorkerSecret': all(r['workerSecretUID'] in (secret_uid, None) for r in rows) and
            all(rows[i]['workerSecretUID'] == secret_uid for i in staged + withdrawn[:1]),
        'stagingObserved': bool(staged),
        'globalWithdrawalObserved': bool(withdrawn),
        'changedDocument': valid_target and bool(original) and original != target,
        'newDocumentPendingThenAcknowledged': bool(pending and acked and min(pending) < max(acked)),
        'workerAcknowledgedBeforeGlobalWithdrawal': bool(acked),
        'ackObservedWhileProjectionStable': bool(stable_acks),
        'nodeReadyThroughTransition': bool(withdrawn) and all(r['nodeReady'] for r in rows[:first + 1]),
        'withdrawalIrreversible': bool(withdrawn) and all(not r['projectionPresent'] for r in rows[first:]),
        'originalMachineEventuallyAbsent': rows[-1]['machinePresent'] is False,
    }
    return {
        'passed': all(checks.values()), 'checks': checks,
        'originalDocumentSHA256': original, 'withdrawnDocumentSHA256': target,
        'firstNewDocumentAt': rows[min(pending)]['at'] if pending else None,
        'firstNewAcknowledgementAt': rows[min(stable_acks)]['at'] if stable_acks else None,
        'globalWithdrawalAt': rows[first]['at'] if withdrawn else None,
        'samples': len(rows),
        'scope': 'Observed changed-document acknowledgement before global withdrawal; requires separate native traffic and provider-cleanup evidence',
    }
