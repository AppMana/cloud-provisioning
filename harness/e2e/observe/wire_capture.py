"""Bounded IPv4 UDP metadata from completed Ethernet or Linux SLL2 pcaps.

Ciphertext hashes identify the same WireGuard datagram across NAT. An inner
packet at tunnel egress alone does not prove that an encrypted datagram was
transmitted. Capture-drop counters must be checked separately.
"""
import argparse
import hashlib
import ipaddress
import json
import struct


def frames(stream):
    header = stream.read(24)
    formats = {b'\xd4\xc3\xb2\xa1': ('<', 1000000),
               b'\xa1\xb2\xc3\xd4': ('>', 1000000),
               b'\x4d\x3c\xb2\xa1': ('<', 1000000000),
               b'\xa1\xb2\x3c\x4d': ('>', 1000000000)}
    if len(header) != 24 or header[:4] not in formats:
        raise ValueError('classic pcap header required')
    endian, resolution = formats[header[:4]]
    major, minor, _, _, snaplen, link = struct.unpack(endian+'HHIIII', header[4:])
    if (major, minor) != (2, 4) or link not in (1, 276) or not 0 < snaplen <= 1048576:
        raise ValueError('bounded Ethernet or SLL2 capture required')
    while True:
        record = stream.read(16)
        if not record:
            return
        if len(record) != 16:
            raise ValueError('incomplete pcap record header')
        seconds, fraction, captured, original = struct.unpack(endian+'IIII', record)
        if fraction >= resolution or captured > snaplen or captured > original:
            raise ValueError('invalid pcap record bounds')
        packet = stream.read(captured)
        if len(packet) != captured:
            raise ValueError('incomplete pcap packet')
        yield seconds*1000000000 + fraction*(1000000000//resolution), link, packet


def udp_metadata(link, packet, *, include_truncated_headers=False):
    if link == 276:
        if len(packet) < 20:
            raise ValueError('incomplete SLL2 header')
        protocol = struct.unpack('!H', packet[:2])[0]
        interface = struct.unpack('!I', packet[4:8])[0]
        direction = packet[10]
        packet = packet[20:]
    else:
        if len(packet) < 14:
            raise ValueError('incomplete Ethernet header')
        protocol = struct.unpack('!H', packet[12:14])[0]
        interface, direction = None, None
        packet = packet[14:]
    if protocol != 0x0800:
        return None
    if len(packet) < 20 or packet[0] >> 4 != 4:
        raise ValueError('invalid IPv4 header')
    ihl = (packet[0] & 15)*4
    total, fragment = struct.unpack('!HH', packet[2:4]+packet[6:8])
    if ihl < 20 or total < ihl or len(packet) < ihl:
        raise ValueError('invalid IPv4 lengths')
    if packet[9] != 17 or fragment & 0x1fff:
        return None
    if total < ihl+8 or len(packet) < ihl+8:
        raise ValueError('incomplete UDP header')
    source, destination, length = struct.unpack('!HHH', packet[ihl:ihl+6])
    if length < 8 or (not fragment & 0x2000 and length > total-ihl):
        raise ValueError('invalid UDP length')
    complete = not fragment & 0x2000 and len(packet) >= total
    row = dict(interface=interface, packetType=direction,
               source=str(ipaddress.ip_address(packet[12:16])), sourcePort=source,
               destination=str(ipaddress.ip_address(packet[16:20])), destinationPort=destination,
               ipID=struct.unpack('!H', packet[4:6])[0], udpBytes=length,
               complete=bool(complete))
    payload = packet[ihl+8:ihl+length]
    if complete or (include_truncated_headers and not fragment & 0x2000):
        # WireGuard padding is capped at the tunnel MTU, so valid data lengths
        # need not be multiples of sixteen (send.c: calculate_skb_padding).
        if (length-8 >= 32 and len(payload) >= 16
                and payload[:4] == b'\x04\0\0\0'):
            row['wireguardReceiver'], row['wireguardCounter'] = struct.unpack('<IQ', payload[4:16])
            if not complete:
                row['wireguardHeaderOnly'] = True
    if complete:
        row['payloadSHA256'] = hashlib.sha256(payload).hexdigest()
        if source == 8081 or destination == 8081:
            token = payload[5:29] if payload.startswith(b'echo ') else payload[:24]
            if len(token) == 24 and all(c in b'0123456789abcdef' for c in token):
                row['probeToken'] = token.decode('ascii')
    return row


def window(stream, start_ns, end_ns, limit=100):
    if type(start_ns) is not int or type(end_ns) is not int or not 0 <= start_ns < end_ns or not 1 <= limit <= 1000:
        raise ValueError('ordered nanosecond window and bounded result limit required')
    rows, matched = [], 0
    for stamp, link, packet in frames(stream):
        if not start_ns <= stamp <= end_ns:
            continue
        row = udp_metadata(link, packet)
        if row is not None:
            matched += 1
            if len(rows) < limit:
                rows.append(dict(timestampNS=stamp, **row))
    return dict(scope='IPv4 UDP; untagged Ethernet or SLL2', matched=matched,
                omitted=matched-len(rows), packets=rows)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('capture')
    p.add_argument('--start-ns', required=True, type=int)
    p.add_argument('--end-ns', required=True, type=int)
    p.add_argument('--limit', type=int, default=100)
    a = p.parse_args()
    with open(a.capture, 'rb') as stream:
        print(json.dumps(window(stream, a.start_ns, a.end_ns, a.limit)))


if __name__ == '__main__':
    main()
