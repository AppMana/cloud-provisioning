"""Generate a bounded, read-only bpftrace transmit observer for Linux hosts.

Run the generated program on the same little-endian host. Retain stdout,
stderr, the generated program and simultaneous packet captures privately.
"""
import argparse
import ipaddress
import json
import pathlib
import re
import sys


def program(interface, peers, seconds):
    if sys.byteorder != 'little':
        raise ValueError('little-endian host required for WireGuard header decoding')
    if not isinstance(interface, str) or not re.fullmatch(r'[A-Za-z0-9_.:-]{1,15}', interface):
        raise ValueError('explicit Linux interface name required')
    if type(seconds) is not int or not 1 <= seconds <= 900:
        raise ValueError('duration must be between 1 and 900 seconds')
    if (not isinstance(peers, list) or not 1 <= len(peers) <= 16 or
            any(not isinstance(peer, str) for peer in peers)):
        raise ValueError('one to sixteen distinct IPv4 peers required')
    addresses = [ipaddress.IPv4Address(peer) for peer in peers]
    if len(set(addresses)) != len(addresses) or any(
            address.is_unspecified or address.is_multicast or
            address == ipaddress.IPv4Address('255.255.255.255') for address in addresses):
        raise ValueError('distinct unicast peer addresses required')
    destinations = ' || '.join(
        f'$dst == 0x{int.from_bytes(address.packed, "little"):08x}'
        for address in addresses)
    template = pathlib.Path(__file__).with_name('wireguard_tx.bt.in').read_text()
    return (template.replace('@@INTERFACE@@', json.dumps(interface))
            .replace('@@DESTINATIONS@@', destinations)
            .replace('@@SECONDS@@', str(seconds)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--interface', required=True)
    parser.add_argument('--peer', action='append', required=True)
    parser.add_argument('--seconds', type=int, required=True)
    args = parser.parse_args()
    try:
        print(program(args.interface, args.peer, args.seconds), end='')
    except ValueError as err:
        parser.error(str(err))


if __name__ == '__main__':
    main()
