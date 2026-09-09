# Observed embedded Flannel

Captured from the single-NIC KVM campaign on 2026-09-06, using k3s
v1.34.1+k3s1 (commit 24fc436e) with its bundled default VXLAN network.
Source: `harness/e2e/.state/defaults-campaign-v4/02-k3s-flannel/observations/`
`remotes-ready-20260906T003201.114542962Z.json`.

The fixture retains only Node names, Flannel annotations, pod CIDRs and kubelet
versions. No separate Flannel DaemonSet or ConfigMap is supplied to the test:
k3s runs Flannel in process. This verifies interpretation of observed resources;
the live campaign separately verifies packet transport and lifecycle behavior.
The k3s release pin identifies the bundle; no standalone Flannel version is
inferred from the outer k3s executable.
