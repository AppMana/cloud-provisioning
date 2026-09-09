#!/usr/bin/env python3
"""Correlate UDP flow observations with nearby native PktMon drop records."""
import argparse
import datetime
import json
import pathlib
import re

HEADER = re.compile(r'::(?P<time>\d{4}-\d\d-\d\d [\d:.]+) \[Microsoft-Windows-PktMon\] (?P<drop>Drop: )?PktGroupId (?P<group>\d+), PktNumber (?P<packet>\d+),.*?Component (?P<component>\d+),')
COMPONENT = re.compile(r'\] Component (\d+), Type [^,]+, Name ([^,]+), (.*)')
IP = re.compile(r'id (\d+), offset (\d+),.*proto UDP \(17\)')
TRAFFIC_CLASS = re.compile(r'tos (0x[0-9a-fA-F]+),')

def parse(text):
    events=[];components={};current=None
    for line in text.splitlines():
        component=COMPONENT.search(line)
        if component:components[int(component[1])]={'driver':component[2],'name':component[3].strip()}
        match=HEADER.search(line)
        if match:
            current={'timestamp':match['time'],'drop':bool(match['drop']),'group':int(match['group']),'packet':int(match['packet']),'component':int(match['component']),'header':line,'body':[]}
            events.append(current)
        elif line.startswith('['):current=None
        elif current is not None:current['body'].append(line)
    return events,components

def correlate(text, source, port, destination, destination_port=8081, window_seconds=0.1):
    events,components=parse(text)
    flow=f'{source}.{port} > {destination}.{destination_port}:'
    signatures=[]
    for event in events:
        if event['drop']:continue
        previous=None;traffic_class=None
        for line in event['body']:
            match=IP.search(line)
            if match:
                previous=int(match[1]);traffic=TRAFFIC_CLASS.search(line)
                traffic_class=int(traffic[1],16) if traffic else None
            if flow in line and previous is not None:
                signatures.append((previous,datetime.datetime.fromisoformat(event['timestamp']),traffic_class))
    drops=[]
    endpoint_pattern=re.compile(re.escape(source)+r'(?:\.\d+)? > '+re.escape(destination)+r'(?:\.\d+)?:')
    for event in events:
        if not event['drop']:continue
        body='\n'.join(event['body'])
        if not endpoint_pattern.search(body):continue
        headers=[IP.search(line) for line in event['body']]
        headers=[h for h in headers if h]
        when=datetime.datetime.fromisoformat(event['timestamp'])
        matches=[h for h in headers if any(int(h[1])==identity and 0 <= (when-start).total_seconds() <= window_seconds for identity,start,_ in signatures)]
        if not matches:continue
        matching_id=int(matches[-1][1])
        previous_classes=sorted({traffic for identity,start,traffic in signatures
            if identity==matching_id and traffic is not None and 0 <= (when-start).total_seconds() <= window_seconds})
        dropped_class=None
        for line in event['body']:
            header=IP.search(line);traffic=TRAFFIC_CLASS.search(line)
            if header and int(header[1])==matching_id and traffic:
                dropped_class=int(traffic[1],16)
        reason=re.search(r'DropReason (.*?), DropLocation (\S+),',event['header'])
        drops.append({k:event[k] for k in ['timestamp','group','packet','component']} | {'componentInfo':components.get(event['component']),'reason':reason[1].strip() if reason else None,'location':reason[2] if reason else None,'ipID':int(matches[-1][1]),'fragmentOffsetBytes':int(matches[-1][2]),'header':event['header'], 'trafficClassesBeforeDrop':previous_classes, 'trafficClassAtDrop':dropped_class, 'trafficClassChanged':bool(previous_classes) and dropped_class is not None and dropped_class not in previous_classes})
    return {'source':source,'sourcePort':port,'destination':destination,'destinationPort':destination_port,'flowObserved':bool(signatures),'correlationWindowSeconds':window_seconds,'drops':drops}

def read_trace(path):
    raw=path.read_bytes()
    return raw.decode('utf-16' if raw.startswith((b'\xff\xfe',b'\xfe\xff')) else 'utf-8-sig')

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('trace',type=pathlib.Path);p.add_argument('--source',required=True);p.add_argument('--port',required=True,type=int);p.add_argument('--destination',required=True);p.add_argument('--destination-port',type=int,default=8081);a=p.parse_args()
    print(json.dumps(correlate(read_trace(a.trace),a.source,a.port,a.destination,a.destination_port),indent=2))

if __name__=='__main__':main()
