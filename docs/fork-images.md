# Candidate networking images

The candidate branches preserve the production branches while validating changes
for the recorded Kubernetes and Calico versions. Install matching Calico CRDs,
Linux components, Windows components, and distro configuration as a unit.
The k0s harness's bundled Calico `3.32.0-0` remains a separate baseline from the
`3.32.1` candidate fork.

## Calico

| Source branch | Base | Candidate changes |
| --- | --- | --- |
| [windows-isolation-v3.31.4](https://github.com/AppMana/forks-calico-windows-ipv6/tree/cloud-provisioning/windows-isolation-v3.31.4) | `3.31.4` | Workload IPv6 host-exemption filtering; Windows endpoint MTU verification and periodic repair |
| [windows-isolation-v3.32.1](https://github.com/AppMana/forks-calico-windows-ipv6/tree/cloud-provisioning/windows-isolation-v3.32.1) | `3.32.1` | Same changes against the newer API/component base |

The workflow publishes `ghcr.io/appmana/node` and `ghcr.io/appmana/cni` using
`<base-tag>-<sanitized-branch>-<12-character-commit>`. Node architecture tags add
`-linux-amd64` or `-windows-ltsc2022`. Native contract tests run on Windows Server
2022 and 2025. The Windows HostProcess image uses the LTSC 2022 packaging base;
this differs from claiming a separate LTSC 2025 application-container image.

Local MTU validation tests pass with 100% coverage of the portable `winmtu`
package. The portable Windows dataplane package records 74.6% on the 3.31 branch
and 74.2% on 3.32. Both complete node executables cross-compile for Windows.
Linux-only HCN mutation fixtures remain Linux tests; MTU and isolation contract
tests also compile for the real Windows API types.

[Published Calico candidate results](validation/calico-branch-images-results.json)
record the registry-verified Windows image for source `2c82c85e8ed1`. Use its
immutable digest when preparing the VM acceptance run. Linux image and combined
manifest completion remain pending for this recorded build.

Both candidates acknowledge workload updates only after HNS policy application
succeeds. The 3.31 branch also backports 3.32's serialized Goldmane statistics
queries after its race detector found concurrent access during bucket rollover.

## kube-proxy

| Kubernetes source branch | Pinned source commit |
| --- | --- |
| [hns-reconcile-v1.34.6](https://github.com/AppMana/forks-kubernetes-kube-proxy-windows-ipv6/tree/cloud-provisioning/hns-reconcile-v1.34.6) | `fbb36641b584f07432649964054986cde373db5f` |
| [hns-reconcile-v1.35.5](https://github.com/AppMana/forks-kubernetes-kube-proxy-windows-ipv6/tree/cloud-provisioning/hns-reconcile-v1.35.5) | `9b00f285777b78a8684700b822b5072f5c5de0a2` |
| [hns-reconcile-v1.36.2](https://github.com/AppMana/forks-kubernetes-kube-proxy-windows-ipv6/tree/cloud-provisioning/hns-reconcile-v1.36.2) | `6d89988d2cc2ddcdd78138fe827b68232c5df8ab` |

The [image-builder branch](https://github.com/AppMana/forks-kube-proxy-calico-hostprocess-ipv6/tree/cloud-provisioning/windows-matrix)
checks out each exact commit, tests `winkernel` on Server 2022 and 2025, and builds
`ghcr.io/appmana/kube-proxy`. Candidate tags append
`-cloud-provisioning-windows-matrix-<12-character-builder-commit>` to the existing
version-specific image tag. Architecture tags append `-windows-ltsc2022`.
The combined manifest uses the matching upstream Linux kube-proxy image.

The executable reports the Kubernetes version plus its source commit. OCI revision
labels identify that commit; the tag identifies the image-builder revision.
The workflow uses a repository-specific read-only deploy key to read the private
Kubernetes fork. The key is stored as `KUBERNETES_SOURCE_DEPLOY_KEY` in the builder
repository's Actions secrets.

The selective HNS reconciliation in the image-builder patches supersedes an older
unconditional-reconcile experiment. Git blame attributes the selective predicate
to `b5bd25ec` and subsequent patch context to `a4429b98`. The source branches carry
the newer implementation so build inputs and reviewed Kubernetes code agree.

[Published kube-proxy candidate results](validation/kube-proxy-branch-images-results.json)
record the successful build workflow, exact Windows and combined-image digests,
and the inspected platform descriptors. These images passed the workflow's
Server 2022 and 2025 contract tests. Cluster VM acceptance remains separate.

## Promotion gates

- Require successful build and test jobs for the exact source and builder commits.
- Resolve and record registry digests before preparing VM images.
- Exercise fresh Server 2022/2025 workers with distro-matched CNI and kube-proxy.
- Verify Service traffic, deny/allow policy, MTU repair, and continuous survivors.
- Repeat CAPI removal/replacement and tunnel endpoint changes.
- Qualify IPv6 in a separate dual-stack VM fleet.

These branches are candidates. Portable tests and image publication are narrower
than the VM acceptance matrix. Production merging remains gated on that evidence.
