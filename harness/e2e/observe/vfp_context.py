"""Decode observed VFP IPv4 flow events; a matching tuple is not packet identity.

The native manifests declare win:IPv4 and win:Port fields, but ToXml emits
their integer storage values. Decode little-endian Windows storage explicitly,
independently of the analysis host's byte order. Keep rule/processing events
separate from forwarding fallback (354), which is not itself a packet drop.
"""
import ipaddress
import json
import re
import xml.etree.ElementTree as ET

NS = '{http://schemas.microsoft.com/win/2004/08/events/event}'
PROVIDER = '9f2660ea-cfe7-428f-9850-aeca612619b0'
FLOW_EVENTS = {101, 110, 115, 600, 604, 608}


def network_integer(value, size):
    number = int(value)
    if not 0 <= number < 1 << (size * 8):
        raise ValueError('Native network field exceeds its declared size')
    return number.to_bytes(size, 'little')


def flow_events(text):
    result = []
    for line in text.lstrip('\ufeff').splitlines():
        if not line.strip():
            continue
        event = ET.fromstring(json.loads(line)['xml'])
        system = event.find(NS + 'System')
        if system is None:
            raise ValueError('Missing native event System')
        provider = system.find(NS + 'Provider')
        if provider is None or provider.get('Guid', '').strip('{}').lower() != PROVIDER:
            continue
        event_id = int(system.findtext(NS + 'EventID'))
        if event_id not in FLOW_EVENTS:
            continue
        version = int(system.findtext(NS + 'Version'))
        if version != 0:
            raise ValueError('Unsupported VFP flow event version')
        fields = {}
        for field in event.findall(NS + 'EventData/' + NS + 'Data'):
            name = field.get('Name')
            if not name or name in fields:
                raise ValueError('Missing or duplicate native field name')
            fields[name] = field.text
        protocol = int(fields['IpProtocol'])
        flow = dict(eventID=event_id, version=version,
            timestamp=system.find(NS + 'TimeCreated').get('SystemTime'),
            source=str(ipaddress.IPv4Address(network_integer(fields['SrcIpv4Addr'], 4))),
            destination=str(ipaddress.IPv4Address(network_integer(fields['DstIpv4Addr'], 4))),
            sourcePort=None if protocol == 252 else int.from_bytes(network_integer(fields['SrcPort'], 2), 'big'),
            destinationPort=None if protocol == 252 else int.from_bytes(network_integer(fields['DstPort'], 2), 'big'),
            protocol=protocol, portName=fields['PortName'], fields=fields)
        if protocol == 252:
            # Native encapsulation-layer events use these slots as selectors:
            # the observed destination value 4096 is not transport port 16.
            flow['encapsulationSelectors'] = dict(source=int(fields['SrcPort']),
                                                  destination=int(fields['DstPort']))
        result.append(flow)
    return result


def inbound_vxlan_creations(text, source, source_port, destination, destination_port, port_name):
    """Return matching newly created UDP flows on one observed DR port.

    The action describes the recorded flow. It does not establish packet
    identity, explain why the classifier chose that action, or prove delivery.
    """
    matches = []
    for flow in flow_events(text):
        fields = flow['fields']
        if (flow['eventID'] != 600 or flow['protocol'] != 17 or
                flow['portName'] != port_name or fields.get('Direction') != '1' or
                fields.get('IsMain') != '1' or fields.get('NumEncaps') != '1' or
                fields.get('EncapType0') != 'VXLAN'):
            continue
        if (flow['source'], flow['sourcePort'], flow['destination'], flow['destinationPort']) != (
                source, source_port, destination, destination_port):
            continue
        action = re.search(r'\bEncapHeaderAction=(\w+)', fields.get('EncapTransposition0') or '')
        matches.append(dict(flow, encapsulationAction=action[1] if action else None))
    return matches
