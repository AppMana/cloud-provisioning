# Observed RKE2 Canal

Captured on 2026-09-06 from a fresh single-NIC KVM site running
RKE2 v1.34.1+rke2r1 with its bundled default Canal. Source:
`harness/e2e/.state/defaults-campaign-v4/03-rke2-canal/observations/rke2-canal`.

The observed images are `rancher/hardened-calico:v3.30.3-build20250909` and
`rancher/hardened-flannel:v0.27.3-build20250901`. The fixture retains the
DaemonSet identity/images, generated Flannel network configuration and Calico
IPPool specification, removing generated metadata and unrelated pod settings.

The Calico pool has both encapsulation modes set to Never, while Canal's actual
transport is Flannel VXLAN. The test preserves that distinction and verifies
resource interpretation only; it does not simulate packet transport.
