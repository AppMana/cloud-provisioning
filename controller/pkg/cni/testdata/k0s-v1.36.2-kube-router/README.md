# Native k0s kube-router resources

Captured on 2026-09-08 from a fresh, isolated single-NIC KVM site running
k0s v1.36.2+k0s.0 and containerd 2.3.2. All five site Nodes were Ready.
The distribution installed kube-router
`quay.io/k0sproject/kube-router:v2.10.0-iptables1.8.13-k0s.0`.

`daemonset.json` retains the native DaemonSet specification and identifying
name/namespace. Runtime metadata and status are omitted. `nodes.json` retains
Node names and allocated pod CIDRs. No CNI manifest was injected by the harness.

These fixtures test CNI detection and native pod-prefix interpretation. They
do not establish packet reachability, remote joins, removal, or replacement.
The older k0s 1.34 fixture remains a separate supported regression observation.
