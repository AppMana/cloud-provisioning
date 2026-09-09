"""Validate completed native udpprobe logs independently of reported summaries."""
import hashlib
import ipaddress
import json
import math


def evaluate(raw, receipt, result, source_ip, max_gap, progress_attempts):
    """Caller must separately bind Pod/container identities and operation ordering.

    progress_attempts is the last live count observed before the operation.
    Stop must be requested after the operation; timestamps alone cannot prove it.
    None validates raw samples only, for a preflight that never became ready;
    it cannot establish an operation window.
    """
    if not math.isfinite(max_gap) or max_gap <= 0 or (progress_attempts is not None and
            (type(progress_attempts) is not int or progress_attempts < 1)):
        raise ValueError('positive preset gap and pre-operation progress required')
    if not raw.endswith(b'\n') or hashlib.sha256(raw).hexdigest() != result['samplesSHA256']:
        raise ValueError('incomplete or changed sample evidence')
    rows = [json.loads(line) for line in raw.splitlines()]
    if not rows or (progress_attempts is not None and len(rows) <= progress_attempts):
        raise ValueError('no sample after pre-operation progress')
    nonce, pid = receipt['nonce'], receipt['pid']
    if (result['nonce'], result['pid']) != (nonce, pid):
        raise ValueError('terminal process identity changed')
    passed, largest, previous_finish, previous_start = 0, 0.0, 0.0, None
    for i, row in enumerate(rows):
        if (row['nonce'], row['pid'], row['sequence'], row['attempts']) != (nonce, pid, i, i + 1):
            raise ValueError('sample identity or sequence changed')
        start, finish = row['startOffsetSeconds'], row['finishOffsetSeconds']
        if not all(math.isfinite(x) for x in (start, finish)) or start < previous_finish or finish < start:
            raise ValueError('invalid monotonic sample times')
        if previous_start is not None:
            largest = max(largest, start - previous_start)
        previous_start, previous_finish = start, finish
        # Source is an IP:port emitted by Go net.UDPConn, including IPv6 brackets.
        address, _, port = row.get('source', '').rpartition(':')
        source_matches = False
        try:
            source_matches = ipaddress.ip_address(address.strip('[]')) == ipaddress.ip_address(source_ip) and 0 < int(port) <= 65535
        except ValueError:
            pass
        if type(row['ok']) is not bool or (row['ok'] and not source_matches):
            raise ValueError('successful sample lacks expected Pod socket identity')
        passed += int(row['ok'])
        healthy = passed == i + 1 and largest <= max_gap
        if (row['passed'] != passed or row['healthy'] != healthy or
                not math.isclose(row['maxStartIntervalSeconds'], largest, abs_tol=1e-8) or
                row['stopBoundary'] != (i == len(rows) - 1)):
            raise ValueError('sample summary or final stop boundary differs')
    ok = passed == len(rows) and largest <= max_gap
    if (not result['stopAcknowledged'] or result['attempts'] != len(rows) or
            result['passed'] != passed or result['ok'] != ok or
            not math.isclose(result['maxStartIntervalSeconds'], largest, abs_tol=1e-8)):
        raise ValueError('terminal summary differs from raw evidence')
    return dict(attempts=len(rows), passed=passed, maxStartIntervalSeconds=largest,
                samplesSHA256=result['samplesSHA256'], ok=ok)
