# Native VFP fixtures

`windows2022-vfp-events.jsonl` and `windows2025-vfp-events.jsonl` contain the first
native event of each selected ID from separate ten-second captures on retained
AWS Windows Server 2022 build 20348 and Server 2025 build 26100 VMs. XML is
unchanged. The accompanying template JSON files contain native provider metadata
for those IDs. Source and fixture hashes are in the
[collector validation](../../../../docs/validation/windows-vfp-collector-results.json).

These background-traffic observations establish the exported schema and byte
order. The known API port 6443 appears as integer 11033; WireGuard port 51820
appears as 27850. IPv4 fields similarly require interpreting the native integer's
little-endian bytes as an IPv4 address. Preserve raw values alongside decoding.

The captures do not reproduce the intermittent UDP failures. Event 354 describes
an inner-to-outer forwarding fallback, not a definitive drop. The fixtures test
decoding and schema rejection; they do not simulate Windows forwarding behavior.

The separate [paired diagnostic](../../../../docs/validation/windows-vfp-paired-results.json)
supplies `windows2025-vfp-unchanged-class-drop.txt`: original Packet Monitor
records for one unfragmented VFP Invalid Packet drop, matching UDP port 59353
and inner IP ID 52069. Traffic class is zero before and at the drop. This extends
the earlier changed-class fixture; it does not identify the cause.
`windows2025-vfp-vxlan-flow.jsonl` contains four unchanged native VFP XML events
for a different failed flow, port 52145, with VXLAN VNI 4096. Creation/deletion
events describe flow state and do not establish packet delivery or loss.

The paired capture's XML export delayed Packet Monitor shutdown and caused its
circular log to overwrite earlier packets. All six correlated native drops
occurred after VFP tracing stopped. Preserve that timing limitation when using
these fixtures; they cannot supply simultaneous VFP context for those drops.

`windows-vfp-decap-cases.json` comes from a subsequent shorter capture with both
traces stopped before conversion. It preserves unchanged native XML for three
failed inbound DR flow creations and six successful neighboring controls on
Windows 2022 and 2025. Failed flows record `EncapHeaderAction=Ignore`; controls
record `Pop`. Its protocol-252 record also verifies that destination selector
4096 is preserved rather than displayed as transport port 16. The
[comparison report](../../../../docs/validation/windows-vfp-decap-context-results.json)
binds fixture hashes to the captured events, native query checks and cleanup.
These tests decode recorded behavior; they do not model the Windows classifier
or prove the cause of the different actions.
