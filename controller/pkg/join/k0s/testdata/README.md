`controlnodes-v1.34.1.json` was captured on 2026-09-05 from the real k0s
v1.34.1+k0s.0 single-NIC VM site with bundled kube-router. The command was
`k0s kubectl get controlnodes.autopilot.k0sproject.io -o json`, reached over
QEMU Guest Agent. Only object identity and `status.k0sVersion` are retained.

The product's previous join provider enumerated GitHub releases using the
KubeletVersion prefix and hit HTTP 403 during remote re-addition. Autopilot
already reported the exact installed release on all three controllers.

`controlnodes-v1.36.4.json` was captured read-only on 2026-10-02 from a
production single-controller k0s v1.36.4+k0s.1 cluster with bundled Calico in
BGP mode, by the same command over its kubeconfig. The controller is renamed
`controller`. Its release suffix is `+k0s.1`, so a join that rebuilt the
version from the kubelet's `v1.36.4+k0s` would install a different build.
