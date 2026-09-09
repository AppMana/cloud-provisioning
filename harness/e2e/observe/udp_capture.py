"""Summarize one UDP exchange in decoded packet appearances, without clock inference.

Input is tshark TSV with frame.number, frame.time_epoch, ip.src, ip.dst,
ip.id, ip.dsfield, udp.srcport, udp.dstport and udp.payload, occurrence=a.
The final occurrence selects the inner datagram when VXLAN is decoded.
"""
import hashlib


def exchange(text, source, source_port, destination, destination_port=8081):
    wanted = (source, destination, str(source_port), str(destination_port))
    reverse = (destination, source, str(destination_port), str(source_port))
    directions = {'request': [], 'reply': []}
    for line in text.splitlines():
        fields = line.split('\t')
        if len(fields) != 9:
            raise ValueError('Require all nine packet-export fields')
        inner = [field.split(',')[-1] for field in fields]
        flow = (inner[2], inner[3], inner[6], inner[7])
        direction = 'request' if flow == wanted else 'reply' if flow == reverse else None
        if direction is None:
            continue
        payload = bytes.fromhex(inner[8])
        directions[direction].append(dict(frame=int(inner[0]), timestamp=inner[1],
            ipID=inner[4], trafficClass=inner[5], payloadSHA256=hashlib.sha256(payload).hexdigest()))
    result = {}
    for name, rows in directions.items():
        result[name] = dict(appearances=len(rows), ipIDs=sorted({row['ipID'] for row in rows}),
            trafficClasses=sorted({row['trafficClass'] for row in rows}),
            payloadSHA256=sorted({row['payloadSHA256'] for row in rows}), frames=[row['frame'] for row in rows])
    return dict(source=source, sourcePort=source_port, destination=destination,
        destinationPort=destination_port, **result,
        scope='Decoded appearances in one capture; absence is not proof of non-delivery and timestamps are not compared across hosts')
