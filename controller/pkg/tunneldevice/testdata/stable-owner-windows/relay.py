#!/usr/bin/env python3
"""Bounded, two-party UDP relay for encrypted native VM experiments.

No keys, payload logs, provisioning, routing changes or third-party destinations.
Each generation gets an independent socket and learned NAT endpoints.
"""
import argparse
import datetime
import json
import os
import pathlib
import selectors
import socket
import time


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--work-dir', type=pathlib.Path, required=True)
    parser.add_argument('--peer', action='append', required=True)
    parser.add_argument('--seconds', type=int, default=300)
    args = parser.parse_args()
    if len(args.peer) != 2 or len(set(args.peer)) != 2 or not 60 <= args.seconds <= 600:
        parser.error('exactly two distinct peer IPv4 addresses and 60..600 seconds required')
    for address in args.peer:
        socket.inet_pton(socket.AF_INET, address)
    os.umask(0o077)
    args.work_dir.mkdir(mode=0o700, parents=False, exist_ok=False)
    stats = {'pid': os.getpid(), 'started': datetime.datetime.now(datetime.timezone.utc).isoformat(),
             'generations': [{'received': [0, 0], 'forwarded': [0, 0], 'rejected': 0} for _ in range(2)]}
    selector = selectors.DefaultSelector()
    endpoints = [{}, {}]
    sockets = []
    try:
        for generation in range(2):
            sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            sockets.append(sock)
            sock.bind(('0.0.0.0', 54871 + generation))
            sock.setblocking(False)
            selector.register(sock, selectors.EVENT_READ, generation)
        (args.work_dir / 'ready.json').write_text(json.dumps(stats))
        deadline = time.monotonic() + args.seconds
        while time.monotonic() < deadline and not (args.work_dir / 'stop').exists():
            for key, _ in selector.select(timeout=0.2):
                payload, source = key.fileobj.recvfrom(65535)
                generation = key.data
                row = stats['generations'][generation]
                if source[0] not in args.peer:
                    row['rejected'] += 1
                    continue
                participant = args.peer.index(source[0])
                row['received'][participant] += 1
                endpoints[generation][participant] = source
                target = endpoints[generation].get(1 - participant)
                if target:
                    key.fileobj.sendto(payload, target)
                    row['forwarded'][participant] += 1
        stats['terminal'] = True
    finally:
        for sock in sockets:
            sock.close()
        selector.close()
        stats['finished'] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        (args.work_dir / 'summary.json').write_text(json.dumps(stats))
    print(json.dumps(stats), flush=True)


if __name__ == '__main__':
    main()
