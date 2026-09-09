"""Find same-host TCP/IP context near a PktMon drop, without asserting identity.

Native Event 1215 can report transport protocol zero for a dropped VXLAN
packet. Use the outer addresses and time window, preserving the reported
protocol. The event has no packet ID; multiple candidates remain ambiguous.
"""
import datetime
import ipaddress
import json
import xml.etree.ElementTree as ET

NS = '{http://schemas.microsoft.com/win/2004/08/events/event}'
PROVIDER = '2f07e2ee-15db-40f1-90ef-9d7ba282188a'


def ipv4_sockaddr(value):
    raw = bytes.fromhex(value)
    if len(raw) != 16 or int.from_bytes(raw[:2], 'little') != 2:
        raise ValueError('Expected native IPv4 SOCKADDR_IN')
    return str(ipaddress.IPv4Address(raw[4:8]))


def events(text):
    result = []
    for line in text.lstrip('\ufeff').splitlines():
        if not line.strip():
            continue
        row = json.loads(line)
        event = ET.fromstring(row['xml'])
        system = event.find(NS + 'System')
        if system is None:
            raise ValueError('Missing native Windows event System')
        provider = system.find(NS + 'Provider')
        if provider is None or provider.get('Guid', '').strip('{}').lower() != PROVIDER:
            continue
        if system.findtext(NS + 'EventID') != '1215':
            continue
        version = int(system.findtext(NS + 'Version'))
        if version not in (1, 2):
            raise ValueError('Unsupported TCP/IP drop event version')
        data = {}
        for field in event.findall(NS + 'EventData/' + NS + 'Data'):
            name = field.get('Name')
            if name in data:
                raise ValueError('Duplicate native event field')
            data[name] = field.text
        if int(data['AddressFamily']) != 2:
            continue
        result.append(dict(eventID=1215, version=version,
            timestamp=system.find(NS + 'TimeCreated').get('SystemTime'),
            source=ipv4_sockaddr(data['SourceAddress']),
            destination=ipv4_sockaddr(data['DestAddress']),
            interfaceIndex=int(data['IfIndex']), reason=int(data['Reason']),
            transportProtocol=int(data['IPTransportProtocol']),
            pathDirection=int(data['PathDirection']), packetCount=int(data['PacketCount'])))
    return result


def nearby(text, timestamp, source, destination, snapshot, window_microseconds=10):
    if window_microseconds <= 0:
        raise ValueError('A positive same-host correlation window is required')
    # PktMon's native timestamps are UTC without a suffix. Normalize both
    # formats; datetime resolution is one microsecond, below this window.
    def time(value):
        parsed = datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))
        return parsed if parsed.tzinfo else parsed.replace(tzinfo=datetime.timezone.utc)
    when = time(timestamp)
    candidates = []
    for event in events(text):
        delta = (when - time(event['timestamp'])).total_seconds() * 1_000_000
        if (event['source'], event['destination']) != (source, destination) or not 0 <= delta <= window_microseconds:
            continue
        index = event['interfaceIndex']
        interfaces = [i for i in snapshot['interfaces'] if i['InterfaceIndex'] == index]
        addresses = [a for a in snapshot['addresses'] if a['InterfaceIndex'] == index]
        candidates.append(dict(**event, precedingMicroseconds=delta,
            interfaces=interfaces, addresses=addresses))
    return dict(candidates=candidates, uniqueCandidate=len(candidates) == 1,
        windowMicroseconds=window_microseconds,
        scope='Same-host outer-address/time candidates; no packet identity or causal proof. Interface metadata is a separate snapshot.')
