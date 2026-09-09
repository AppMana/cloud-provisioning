# Join patterns

One file per cluster distribution: the cloud-init a remote machine
boots with, rendered by the join reconciler and executed by the
infrastructure provider. The cloud-init patterns use shared file and command blocks. SCOS/OKD needs
Ignition and a separate bootstrap-format implementation.

## Who balances the API path

A remote node must not depend on any single control plane. Which
component provides that is a property of the distribution, so it is
tracked here, and the pattern for each distribution uses the right
one rather than stacking two balancers or leaving none:

| distribution | node-local balancing | this operator's balancer (`--api-proxy-port`) |
|---|---|---|
| kubeadm | none: kubelet dials the join endpoint forever | **on** (default 7445): join and kubelet dial `127.0.0.1`, the dialer's host unit forwards to whichever control plane answers. Requires the loopback address in the API servers' certificate SANs (`certSANs`) |
| k0s | `nodeLocalLoadBalancing` (opt-in in the k0s spec; this operator's target sites enable it) | off: k0s workers balance for themselves once nllb is enabled. A k0s site without nllb has the kubeadm problem and should enable it there, not here |
| k3s | built into the agent: a client-side load balancer across all servers, maintained automatically after registration (state in `<data-dir>/agent/etc/k3s-agent-load-balancer.json`) | off: the pattern never references the balancer port, and a render test pins that |
| RKE2 | agent balancer, registering through supervisor port 9345 | off |
| MicroK8s | native worker API proxy on 16443 | off; join credentials come from native `microk8s add-node` |
| Talos | KubePrism, `127.0.0.1:7445`, enabled by default on current releases | excluded from the pattern matrix, by decision: the tunnel is host-configured *because* a worker's tunnels must not rely on the Kubernetes they carry, and Talos runs no foreign binaries and has no systemd to host that floor. Supporting Talos is a different product surface (WireGuard in the machine config, or a system extension), not a join pattern; this row is the tracking record for that open design question |

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

- Cloud-init patterns: systemd, curl, sha256sum, a kernel with the
  WireGuard module (mainline since 5.6; present on current
  Debian/Ubuntu, RHEL 9+, Fedora, Amazon Linux 2's 5.10, Arch, and
  the Azure/GCP default images; absent on RHEL 8's 4.18 without
  elrepo). The dialer needs no package beyond the binary the pattern
  verifies and installs.
- kubeadm: kubeadm, kubelet, and a container runtime must already be
  on the image; the pattern deliberately installs no packages, because
  package-manager syntax is the least portable thing a cloud-config
  can contain and the machine template already chooses the image.
  Kubelet extra args are written to both families' environment files
  (/etc/default/kubelet and /etc/sysconfig/kubelet), because each
  family's kubelet drop-in reads only its own.
- k0s: self-installs from k0s's pinned release download, so the image
  needs nothing Kubernetes-related at all.
- k3s and RKE2: the same self-install property, from get.k3s.io and
  get.rke2.io, pinned to the cluster's own version (a k3s/RKE2 kubelet
  reports the full install version, so the provider pins it verbatim).
  The token is k3s's secure format, minted through the API by
  pkg/join/k3s (RKE2 is a flavor of the same provider; see its package
  doc for the source-verified equivalence).

## Which images machine templates should name

The image requirement is the join provider's, not this operator's, so
the machine template's image follows from which provider the cluster
selected:

- **k0s** (and any future self-installing provider): a stock distro
  cloud image, current generation, nothing Kubernetes on it. AWS:
  Canonical's Ubuntu 24.04 LTS AMIs (owner 099720109477), Debian 12
  (owner 136693071363), or AL2023. GCP: debian-cloud/debian-12 (the
  platform default) or ubuntu-os-cloud/ubuntu-2404-lts. Azure:
  Canonical ubuntu-24_04-lts. Every one of these carries systemd,
  cloud-init, curl, sha256sum, and a WireGuard-capable kernel. Zero
  image maintenance is part of why k0s is the production choice here.
- **k3s / RKE2** (self-installing, like k0s): the same stock distro
  cloud images as the k0s row, nothing Kubernetes on them. RKE2's
  agent downloads more at first start (its components run as images
  its containerd pulls), which is startup time, not an image
  requirement.
- **kubeadm**: an image built with kubernetes-sigs/image-builder (the
  Cluster API standard), pinned to a Kubernetes version matching the
  cluster's -- JoinValues discovers kubernetesVersion for exactly this
  comparison. CAPA's community AMIs exist for the newest three minor
  series only, are deleted as series age out, and are explicitly not
  recommended for production: lab and development only. Building and
  retaining your own image-builder AMIs per version is the supported
  path, and it is image maintenance the self-installing rows simply
  do not have.

## MicroK8s configuration

Select `joinProvider: microk8s` and supply Secret
`cloud-provisioning/microk8s-provider-config`, key `config.json`:

```json
{
  "joinURL": "10.0.0.10:25000/<native-token>/<certificate-check>",
  "revision": "9063",
  "version": "v1.34.9",
  "expiresAt": "<RFC3339 expiration>"
}
```

Obtain the native URL with `microk8s add-node --token-ttl 14400 --format json`
on a control-plane host. Set expiration from the issuance time and actual TTL;
renew the Secret before expiration. The provider refuses credentials with less
than five minutes remaining and requires the configured version to match the
registered nodes. This credential is managed by MicroK8s's cluster agent, not a
Kubernetes bootstrap-token Secret.

The pattern writes a native launch configuration before installing the pinned
snap revision, requests a worker join, and lets MicroK8s install bundled Calico.
The current machine image must support apt, snapd, Python 3, systemd and cloud-init.

The kubelet advertises the worker's allocated WireGuard address. Control planes
must reach this address on TCP 10250 for Kubernetes exec and logs. The cloud's
private NIC address can be unreachable from the site or overlap another cloud;
it must not replace the tunnel address in MicroK8s's kubelet arguments. The
infrastructure provider ID remains separate and identifies the actual Machine.
