# Native HNS layer fixtures

`windows2022-hns-layers.xml` and `windows2025-hns-layers.xml` contain native
`tracerpt` event elements selected from the paired single-NIC AWS VM capture.
The `Events` wrapper is fixture packaging. Selected event elements preserve
the decoded native text. The capture, source hashes, fixture hashes, port
identities and cleanup are recorded in the
[layer-gap validation](../../../../docs/validation/windows-hns-layer-gap-results.json).

The fixtures include encapsulation and unrelated layer operations, timer
callbacks, reused activity IDs, and non-HNS decoding errors. Windows 2022 has
nine complete encapsulation-layer rebuilds on the selected DR port; Windows
2025 has five. Activity IDs do not uniquely identify a single rebuild.

`windows-hns-decap-cases.json` binds six native packet-drop headers to unchanged
VFP new-flow XML and the corresponding host/port. Each matched flow records
`EncapHeaderAction=Ignore` between HNS layer removal and addition. The fixture
checks decoding and temporal correlation; it does not simulate HNS, prove a
corrective change, or qualify continuous traffic.

`windows2022-hns-undecoded.xml` preserves a payload-free native HNS record
returned by `Get-WinEvent` with `ProcessingErrorData`. Its source is the
[HNS collector validation](../../../../docs/validation/windows-hns-collector-results.json)
(`2022-hns-events.jsonl.gz`). It verifies that an undecoded HNS record cannot
silently become an empty analysis result. Unrelated provider errors in the
layer fixtures do not prevent analysis of fully decoded HNS records.

Negative tests mutate native records to exercise missing context, duplicate
fields, incomplete captures and reversed event order. Those mutations are
synthetic validation cases, not additional cluster observations.

`windows2022-hns-deferred-spaces.xml` preserves 15 unchanged native event
elements from the overlay-removal capture with a stable site-node address.
`windows-hns-deferred-spaces.json` records the source and fixture hashes,
the native API-removal result and the failed reply timestamp. HNS removes
the deleted network's IPv6 and IPv4 VFP spaces more than 30 seconds after
API deletion, immediately after the reply drop. Adjacent additions belong
to other spaces or ports and must not be mistaken for these removals.
The `network_space_removals` observer reports this deferred work; neither
removal events nor an empty result prove that the network has converged.
