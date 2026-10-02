# k0s bundled Calico, BGP without encapsulation, dual-stack

Captured read-only on 2026-10-02 from a production three-node k0s
v1.36.4+k0s.1 cluster (one controller+worker, two workers) running the bundled
Calico v3.32.1-3 with `mode: bird`, `overlay: Never` and IPv4/IPv6 pools. No
remote worker was attached.

Capture: `kubectl get ippools,blockaffinities,controlnodes,nodes -o json` and
the `calico-node` DaemonSet, through `jq` keeping only names and the fields the
controller reads (pool and block specs, Calico node address annotations,
InternalIPs, kubelet version and architecture, the calico-node environment and
the `install-cni` network configuration). Host names were replaced with
`controller`, `worker-1` and `worker-2`; the Calico interface inventory and
Hostname addresses were removed. Addresses and CIDRs are unchanged.

The DaemonSet environment is as deployed, including k0s's duplicated MTU
variables (the later value wins) and its address autodetection CIDRs. The pod
interface MTU comes from the CNI configuration (`"mtu": 1450`), not the Felix
encapsulation MTUs, because no pool encapsulates.

The worker IPv6 BGP addresses are SLAAC addresses inside the autodetection CIDR,
not the workers' InternalIPs. That is what the cluster records, so it stays.
