`controlnodes-v1.34.1.json` was captured on 2026-09-05 from the real k0s
v1.34.1+k0s.0 single-NIC VM site with bundled kube-router. The command was
`k0s kubectl get controlnodes.autopilot.k0sproject.io -o json`, reached over
QEMU Guest Agent. Only object identity and `status.k0sVersion` are retained.

The product's previous join provider enumerated GitHub releases using the
KubeletVersion prefix and hit HTTP 403 during remote re-addition. Autopilot
already reported the exact installed release on all three controllers.
