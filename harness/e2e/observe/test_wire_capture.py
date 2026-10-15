import io
import struct
import unittest

from wire_capture import window, udp_metadata


def packet(payload, source=b'\xac\x1d\0\x1d', link=276):
    udp = struct.pack('!HHHH', 50000, 51820, 8+len(payload), 0)+payload
    ip = struct.pack('!BBHHHBBH4s4s', 0x45, 0, 20+len(udp), 12, 0x4000, 64, 17, 0,
                     source, b'\x0a\x65\0\x02')+udp
    if link == 276:
        return struct.pack('!HHIHBB8s', 0x800, 0, 7, 1, 4, 6, b'\0'*8)+ip
    return b'\0'*12+struct.pack('!H', 0x800)+ip


def capture(packets, *, link=276, endian='<', nano=False, original_extra=0):
    magic = (b'\x4d\x3c\xb2\xa1' if nano else b'\xd4\xc3\xb2\xa1')
    if endian == '>':
        magic = magic[::-1]
    data = magic+struct.pack(endian+'HHIIII', 2, 4, 0, 0, 1024, link)
    for body in packets:
        data += struct.pack(endian+'IIII', 10, 123, len(body), len(body)+original_extra)+body
    return data


class CaptureTest(unittest.TestCase):
    def test_ciphertext_identity_survives_nat_and_link_encoding(self):
        payload = struct.pack('<IIQ', 4, 321, 987654)+b'x'*32
        a = window(io.BytesIO(capture([packet(payload)])), 0, 20_000_000_000)['packets'][0]
        b = window(io.BytesIO(capture([packet(payload, b'\xc0\0\x02\x01', 1)], link=1)), 0, 20_000_000_000)['packets'][0]
        self.assertNotEqual(a['source'], b['source'])
        self.assertEqual(a['payloadSHA256'], b['payloadSHA256'])
        self.assertEqual((a['interface'], a['packetType']), (7, 4))
        self.assertEqual((a['wireguardReceiver'], a['wireguardCounter']), (321, 987654))

    def test_clock_resolution_and_byte_order(self):
        for endian in ['<', '>']:
            for nano in [False, True]:
                with self.subTest(endian=endian, nano=nano):
                    raw = capture([packet(b'x')], endian=endian, nano=nano)
                    r = window(io.BytesIO(raw), 0, 20_000_000_000)
                    self.assertEqual(r['packets'][0]['timestampNS'], 10_000_000_000+123*(1 if nano else 1000))

    def test_snaplen_truncation_never_produces_ciphertext_identity(self):
        body = packet(struct.pack('<IIQ', 4, 1, 2)+b'x'*32)
        raw = capture([body[:-4]], original_extra=4)
        row = window(io.BytesIO(raw), 0, 20_000_000_000)['packets'][0]
        self.assertFalse(row['complete'])
        self.assertNotIn('payloadSHA256', row)
        self.assertNotIn('wireguardCounter', row)

    def test_truncated_header_requires_explicit_opt_in_and_never_has_hash(self):
        payload = struct.pack('<IIQ', 4, 321, 987654)+b'x'*256
        for link, header in [(1, 14), (276, 20)]:
            body = packet(payload, link=link)
            cut = header+20+8+16
            row = udp_metadata(link, body[:cut], include_truncated_headers=True)
            self.assertTrue(row['wireguardHeaderOnly'])
            self.assertFalse(row['complete'])
            self.assertEqual((row['wireguardReceiver'], row['wireguardCounter']), (321, 987654))
            self.assertNotIn('payloadSHA256', row)
            self.assertNotIn('wireguardCounter', udp_metadata(link, body[:cut]))
            self.assertNotIn('wireguardCounter', udp_metadata(link, body[:cut-1], include_truncated_headers=True))

    def test_header_opt_in_rejects_fragments_and_invalid_data_lengths(self):
        for payload in [struct.pack('<IIQ', 4, 1, 2)+b'x'*15,
                        struct.pack('<IIQ', 3, 1, 2)+b'x'*32]:
            self.assertNotIn('wireguardCounter', udp_metadata(276, packet(payload), include_truncated_headers=True))
        body = bytearray(packet(struct.pack('<IIQ', 4, 1, 2)+b'x'*32))
        body[26:28] = struct.pack('!H', 0x2000)
        self.assertNotIn('wireguardCounter', udp_metadata(276, bytes(body), include_truncated_headers=True))

    def test_mtu_capped_padding_need_not_align_to_sixteen_bytes(self):
        body = packet(struct.pack('<IIQ', 4, 321, 987654)+b'x'*1436)
        complete = udp_metadata(276, body)
        self.assertEqual(complete['wireguardCounter'], 987654)
        self.assertIn('payloadSHA256', complete)
        short = udp_metadata(276, body[:192], include_truncated_headers=True)
        self.assertEqual(short['wireguardCounter'], 987654)
        self.assertTrue(short['wireguardHeaderOnly'])
        self.assertNotIn('payloadSHA256', short)

    def test_missing_file_bytes_and_invalid_bounds_fail(self):
        raw = capture([packet(b'x')])
        for broken in [raw[:12], raw[:-1], raw+b'x', raw[:24]+struct.pack('<IIII', 10, 0, 1025, 1025)]:
            with self.subTest(length=len(broken)), self.assertRaises(ValueError):
                window(io.BytesIO(broken), 0, 20_000_000_000)

    def test_omitted_packets_are_explicit(self):
        raw = capture([packet(b'x')]*3)
        result = window(io.BytesIO(raw), 0, 20_000_000_000, 1)
        self.assertEqual((result['matched'], result['omitted'], len(result['packets'])), (3, 2, 1))
        self.assertEqual(window(io.BytesIO(raw), 0, 1)['matched'], 0)

    def test_fragment_is_not_a_complete_datagram(self):
        body = bytearray(packet(b'x'*40))
        body[26:28] = struct.pack('!H', 0x2000)
        result = window(io.BytesIO(capture([body])), 0, 20_000_000_000)
        self.assertFalse(result['packets'][0]['complete'])
        body[26:28] = struct.pack('!H', 1)
        self.assertEqual(window(io.BytesIO(capture([body])), 0, 20_000_000_000)['matched'], 0)

    def test_probe_payload_token_matches_request_and_response(self):
        token = b'1234567890abcdef00000000'
        for payload in [b'echo '+token+b'x'*40, token+b'x'*40]:
            body = bytearray(packet(payload))
            body[42:44] = struct.pack('!H', 8081)
            result = window(io.BytesIO(capture([body])), 0, 20_000_000_000)
            self.assertEqual(result['packets'][0]['probeToken'], token.decode())
