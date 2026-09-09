import pathlib
import tempfile
import unittest
import pktmon

ROOT=pathlib.Path(__file__).parent/'testdata'
class NativeDropCorrelationTest(unittest.TestCase):
    def test_native_vfp_invalid_packet_does_not_require_class_change(self):
        text=(ROOT/'windows2025-vfp-unchanged-class-drop.txt').read_text()
        drops=pktmon.correlate(text,'10.244.114.4',59353,'10.244.42.6')['drops']
        self.assertEqual(len(drops),1)
        drop=drops[0]
        self.assertEqual((drop['componentInfo']['driver'],drop['reason'],drop['location']),
                         ('vfpext.sys','Invalid Packet','0xE0006096'))
        self.assertEqual((drop['ipID'],drop['fragmentOffsetBytes']),(52069,0))
        self.assertEqual(drop['trafficClassesBeforeDrop'],[0])
        self.assertEqual(drop['trafficClassAtDrop'],0)
        self.assertFalse(drop['trafficClassChanged'])

    def test_native_dual_receivers_drop_requests_without_traffic_class_change(self):
        for version, source, port, dest in [('2022','10.244.42.6',65024,'10.244.114.4'),
                                             ('2025','10.244.114.4',59926,'10.244.42.6')]:
            with self.subTest(version=version):
                text=(ROOT/('windows'+version+'-dual-request-drop.txt')).read_text()
                drops=pktmon.correlate(text,source,port,dest)['drops']
                self.assertEqual(len(drops),1)
                self.assertEqual(drops[0]['componentInfo']['driver'],'tcpip.sys')
                self.assertEqual(drops[0]['reason'],'Not locally destined')
                self.assertEqual(drops[0]['trafficClassAtDrop'],0)
                self.assertFalse(drops[0]['trafficClassChanged'])

    def test_native_stream_drops_keep_tcpip_and_vfp_distinct(self):
        text=(ROOT/'windows2022-native-stream-drops.txt').read_text()
        vfp=pktmon.correlate(text,'10.244.42.6',55823,'10.244.114.4')['drops']
        tcpip=pktmon.correlate(text,'10.244.190.81',43734,'10.244.114.4')['drops']
        self.assertEqual(len(vfp),1);self.assertEqual(len(tcpip),1)
        self.assertEqual((vfp[0]['componentInfo']['driver'],vfp[0]['reason']),('vfpext.sys','Invalid Packet'))
        self.assertEqual((tcpip[0]['componentInfo']['driver'],tcpip[0]['reason']),('tcpip.sys','Not locally destined'))
        self.assertEqual(vfp[0]['trafficClassesBeforeDrop'],[0])
        self.assertEqual(vfp[0]['trafficClassAtDrop'],0x74)
        self.assertTrue(vfp[0]['trafficClassChanged'])
        self.assertFalse(tcpip[0]['trafficClassChanged'])
        self.assertEqual(pktmon.correlate(text,'10.244.42.6',55824,'10.244.114.4')['drops'],[])

    def test_unfragmented_native_vxlan_reply_drop(self):
        text=(ROOT/'windows2022-small-vxlan-reply-drop.txt').read_text()
        result=pktmon.correlate(text,'10.244.190.81',8081,'10.244.114.4',58049)
        self.assertTrue(result['flowObserved'])
        self.assertEqual(len(result['drops']),1)
        drop=result['drops'][0]
        self.assertEqual((drop['ipID'],drop['fragmentOffsetBytes']),(13186,0))
        self.assertEqual(drop['componentInfo']['driver'],'vfpext.sys')
        self.assertEqual((drop['reason'],drop['location']),('Invalid Packet','0xE0006096'))
        self.assertEqual(pktmon.correlate(text,'10.244.190.81',8081,'10.244.114.4',58050)['drops'],[])

    def test_native_reply_drop_uses_ephemeral_destination_port(self):
        text=(ROOT/'windows2025-udp-response-drop.txt').read_text()
        response=pktmon.correlate(text,'10.244.42.5',8081,'10.244.114.4',61651)
        self.assertTrue(response['flowObserved'])
        self.assertEqual([d['fragmentOffsetBytes'] for d in response['drops']],[0,1344])
        self.assertEqual({d['location'] for d in response['drops']},{'0xE0006096'})
        request=pktmon.correlate(text,'10.244.114.4',61651,'10.244.42.5')
        self.assertFalse(request['flowObserved'])
        self.assertEqual(request['drops'],[])

    def test_both_native_formats_correlate_fragments_despite_group_and_port_changes(self):
        for version,source,port,dest in [('2022','10.244.167.145',63113,'10.244.233.145'),('2025','10.244.233.145',51665,'10.244.190.80')]:
            with self.subTest(version=version):
                result=pktmon.correlate((ROOT/('windows'+version+'-udp-drop.txt')).read_text(),source,port,dest)
                self.assertTrue(result['flowObserved'])
                self.assertEqual([d['fragmentOffsetBytes'] for d in result['drops']],[0,1344])
                self.assertEqual({d['componentInfo']['driver'] for d in result['drops']},{'vfpext.sys'})
                self.assertEqual({d['reason'] for d in result['drops']},{'Invalid Packet'})

    def test_fragment_bad_length_annotation_alone_is_not_a_drop(self):
        text=(ROOT/'windows2022-udp-drop.txt').read_text()
        text=text[:text.index('[00]2024.2200::2026-09-07 11:31:43.212519600')]
        self.assertIn('bad length',text)
        self.assertEqual(pktmon.correlate(text,'10.244.167.145',63113,'10.244.233.145')['drops'],[])

    def test_other_flow_and_late_reused_ip_id_do_not_match(self):
        text=(ROOT/'windows2022-udp-drop.txt').read_text()
        self.assertFalse(pktmon.correlate(text,'10.244.167.145',12345,'10.244.233.145')['flowObserved'])
        self.assertEqual(pktmon.correlate(text,'10.244.167.145',12345,'10.244.233.145')['drops'],[])
        text=text.replace('11:31:43.212519600','11:32:43.212519600').replace('11:31:43.212520100','11:32:43.212520100')
        self.assertEqual(pktmon.correlate(text,'10.244.167.145',63113,'10.244.233.145')['drops'],[])

    def test_native_utf16_and_exported_utf8_are_supported(self):
        text=(ROOT/'windows2022-udp-drop.txt').read_text()
        with tempfile.TemporaryDirectory() as directory:
            p=pathlib.Path(directory)/'capture.txt'
            for encoding in ['utf-16','utf-8-sig']:
                p.write_bytes(text.encode(encoding))
                self.assertEqual(pktmon.read_trace(p),text)

if __name__=='__main__':unittest.main()
