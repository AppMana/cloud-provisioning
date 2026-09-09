# Observed kubeadm native Calico

Captured on 2026-09-06 from the single-NIC KVM campaign's kubeadm v1.34.0
explicit Calico regression profile. Source:
`harness/e2e/.state/defaults-campaign-v4/04-kubeadm-calico/observations/kubeadm-calico`.
Kubeadm does not bundle a CNI; this profile installs Calico with native routing.

Only resource identities and IPPool/BlockAffinity specifications are retained.
The remote blocks were captured after probe pods caused Calico to allocate
addresses. The test checks that the detector publishes these observed blocks,
not an invented fixed node subdivision. The block values are observations of
this run, not an allocation guarantee for future runs. Packet reachability is
verified separately by the live campaign.
