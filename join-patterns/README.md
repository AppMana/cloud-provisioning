# Join patterns

One file per cluster distribution: the cloud-init a remote machine
boots with, rendered by the join reconciler and executed by the
infrastructure provider. Only `write_files` and `runcmd` may appear
(see the header of either pattern for why).

## Who balances the API path

A remote node must not depend on any single control plane. Which
component provides that is a property of the distribution, so it is
tracked here, and the pattern for each distribution uses the right
one rather than stacking two balancers or leaving none:

| distribution | node-local balancing | this operator's balancer (`--api-proxy-port`) |
|---|---|---|
| kubeadm | none: kubelet dials the join endpoint forever | **on** (default 7445): join and kubelet dial `127.0.0.1`, the dialer's host unit forwards to whichever control plane answers. Requires the loopback address in the API servers' certificate SANs (`certSANs`) |
| k0s | `nodeLocalLoadBalancing` (opt-in in the k0s spec; this operator's target sites enable it) | off: k0s workers balance for themselves once nllb is enabled. A k0s site without nllb has the kubeadm problem and should enable it there, not here |
| k3s | built into the agent: a client-side load balancer across all servers, maintained automatically after registration | off (pattern not yet written) |
| RKE2 | same agent balancer as k3s | off (pattern not yet written) |
| Talos | KubePrism, `127.0.0.1:7445`, enabled by default on current releases | off, and the whole pattern differs: Talos has no systemd and runs no foreign binaries, so the tunnel itself must come from the machine config's own WireGuard support or a system extension, not this dialer. Open design question |

`--join-api-proxy-port=0` turns the operator's balancer off for a
kubeadm-family cluster that terminates its API behind an external
load balancer and wants nodes pinned to it.

The lab's own site nodes get the same property from a static-pod TCP
forwarder written before kubeadm runs (see `harness/clab/cluster.sh`):
kubelet starts static pods from disk with no API access, which is the
same reason the remote's balancer lives in the dialer's host unit and
never in a pod.

## Distribution assumptions

What each pattern requires of the machine image, kept explicit so a
new distribution's gaps are found by reading rather than by a node
that joins and is quietly wrong:

- Both patterns: systemd, curl, sha256sum, a kernel with the
  WireGuard module (mainline since 5.6; present on current
  Debian/Ubuntu, RHEL 9+, Fedora, Amazon Linux 2's 5.10, Arch, and
  the Azure/GCP default images; absent on RHEL 8's 4.18 without
  elrepo). The dialer needs no package beyond the binary the pattern
  verifies and installs.
- kubeadm: kubeadm and kubelet must already be on the image; the
  pattern deliberately installs no packages, because package-manager
  syntax is the least portable thing a cloud-config can contain and
  the machine template already chooses the image. Kubelet extra args
  are written to both families' environment files
  (/etc/default/kubelet and /etc/sysconfig/kubelet), because each
  family's kubelet drop-in reads only its own.
- k0s: self-installs from k0s's pinned release download, so the image
  needs nothing Kubernetes-related at all.
