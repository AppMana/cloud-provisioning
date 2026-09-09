Captured 2026-09-05 from a real five-node single-NIC VM site.

- k0s: v1.34.1+k0s.0 (Kubernetes v1.34.1+k0s)
- Bundled Calico image: quay.io/k0sproject/calico-node:v3.29.6-0
- Configuration: distribution-managed Calico, default VXLAN. No standalone CNI installer applied.
- Capture: `k0s kubectl get ippools.crd.projectcalico.org -o json` and
  `k0s kubectl get blockaffinities.crd.projectcalico.org -o json` over QEMU Guest Agent.
- All five site nodes were Ready. Remote lifecycle and outage behavior are not asserted by these fixtures.
- Volatile object metadata removed; resource specs retained.
