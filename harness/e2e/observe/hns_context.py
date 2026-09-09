"""Pair native HNS layer rebuild records from one host's tracerpt XML.

Intervals describe observed control operations, not packet identity or causality.
Activity IDs may be reused; pair each removal with the next matching addition.
"""
import datetime
import xml.etree.ElementTree as ET

NS = '{http://schemas.microsoft.com/win/2004/08/events/event}'
PROVIDER = '0c885e0d-6eb6-476c-a048-2457eed3a5c1'


def _native_events(text):
    for event in ET.fromstring(text):
        system = event.find(NS + 'System')
        if system is None:
            raise ValueError('Missing native event System')
        provider = system.find(NS + 'Provider')
        if provider is None or provider.get('Guid', '').strip('{}').lower() != PROVIDER:
            continue
        if event.find(NS + 'ProcessingErrorData') is not None:
            raise ValueError('Undecoded HNS event; use native tracerpt and verify provider counts')
        yield event, system


def _fields(event):
    fields = {}
    for field in event.findall(NS + 'EventData/' + NS + 'Data'):
        name = field.get('Name')
        if not name or name in fields:
            raise ValueError('Missing or duplicate HNS field name')
        fields[name] = field.text
    return fields


def layer_rebuilds(text, port, layer='VNET_ENCAP_LAYER'):
    pending = {}
    result = []
    previous = None
    for event, system in _native_events(text):
        task = event.findtext(NS + 'RenderingInfo/' + NS + 'Task')
        if task not in ('RemovingLayer', 'AddingLayer'):
            continue
        fields = _fields(event)
        if not fields.get('portId') or not fields.get('layerId'):
            raise ValueError('Missing native layer or port ID')
        if fields.get('portId', '').lower() != port.lower() or fields.get('layerId') != layer:
            continue
        correlation = system.find(NS + 'Correlation')
        activity = correlation.get('ActivityID') if correlation is not None else None
        if not activity:
            raise ValueError('Missing layer-operation activity ID')
        timestamp = system.find(NS + 'TimeCreated').get('SystemTime')
        when = datetime.datetime.fromisoformat(timestamp.replace('Z', '+00:00'))
        if when.tzinfo is None:
            raise ValueError('Expected a timezone in native event timestamp')
        if previous is not None and when < previous:
            raise ValueError('Layer records are not in event-time order')
        previous = when
        key = activity.lower()
        if task == 'RemovingLayer':
            if key in pending:
                raise ValueError('Repeated layer removal without a matching addition')
            pending[key] = (timestamp, when)
        elif key in pending:
            removed_at, removed_time = pending.pop(key)
            result.append(dict(activity=activity, port=port, layer=layer,
                removedAt=removed_at, addedAt=timestamp,
                durationMicroseconds=round((when - removed_time).total_seconds() * 1_000_000),
                complete=True))
    for activity, (timestamp, _) in pending.items():
        result.append(dict(activity=activity, port=port, layer=layer,
            removedAt=timestamp, addedAt=None, durationMicroseconds=None, complete=False))
    return result


def network_space_removals(text, port, network_id):
    """Find removal of one network's IPv4/IPv6 VFP spaces on an exact port.

    HNS API deletion may precede these operations. These records identify
    deferred cleanup, not successful completion or a globally settled network.
    An empty result is not evidence that no pending dataplane state exists.
    """
    spaces = {network_id.lower(): 'ipv4', network_id.lower() + '_v6': 'ipv6'}
    result = []
    previous = None
    for event, system in _native_events(text):
        if event.findtext(NS + 'RenderingInfo/' + NS + 'Task') != 'RemovingSpace':
            continue
        fields = _fields(event)
        if not fields.get('portId') or not fields.get('spaceId'):
            raise ValueError('Missing native space or port ID')
        space = fields['spaceId'].lower()
        if fields['portId'].lower() != port.lower() or space not in spaces:
            continue
        timestamp = system.find(NS + 'TimeCreated').get('SystemTime')
        when = datetime.datetime.fromisoformat(timestamp.replace('Z', '+00:00'))
        if when.tzinfo is None:
            raise ValueError('Expected a timezone in native event timestamp')
        if previous is not None and when < previous:
            raise ValueError('Space records are not in event-time order')
        previous = when
        result.append(dict(port=fields['portId'], space=fields['spaceId'],
                           family=spaces[space], removedAt=timestamp))
    return result
