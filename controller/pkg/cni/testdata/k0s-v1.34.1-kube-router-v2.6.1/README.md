Captured 2026-09-05 from a fresh five-node single-NIC VM site using
k0s v1.34.1+k0s.0's built-in `provider: kuberouter`.

The bundled image was `quay.io/k0sproject/kube-router:v2.6.1-iptables1.8.11-0`.
All five site nodes and all five kube-router pods were Ready. No standalone
kube-router installer was applied. `daemonset.json` retains the observed pod
spec; metadata and status were stripped. `nodes.json` retains names and the
allocated pod CIDRs used by the detector. This fixture does not prove remote
lifecycle or BGP/datapath recovery.
