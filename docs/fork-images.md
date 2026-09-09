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
record the successful workflow and registry-verified images for source
`9e4adf0f9920`. Its combined manifest and immutable Windows image configuration
both specify OS version `10.0.20348.5499`. The Windows cache set pins this image;
the evidence record also includes matching Linux node and CNI digests.

The earlier source `2c82c85e8ed1` published a combined manifest with OS version
`10.0.20348.5622`, which differed from its built image. Both candidate branches
now derive that field from the built image configuration. The earlier publisher
queried a moving Nano Server tag. The superseded record retains the mismatch.

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

## Windows acceptance image set

The [k0s 1.36 / Calico 3.32 candidate image set](../images/windows/candidates/k0s-1.36-calico-3.32.json)
pins the Calico node, matching upstream Windows CNI installer, and forked
Kubernetes 1.36.2 kube-proxy. The installer digest selects the LTSC 2022
`windows/amd64` manifest. The node image also carries the fork's CNI executables;
its node service copies those binaries to the host after installer setup.

Use the shared set when preparing each Windows image:

```sh
python3 images/windows/cache.py --runtime runtime.json --windows-version 2022 \
  --image-file images/windows/candidates/k0s-1.36-calico-3.32.json \
  --output windows-2022-network-cache.json
```

Repeat with `--windows-version 2025` and a different output path. Add the complete
sandbox and workload image set with additional `--image-file` or `--image` options.
These three networking images alone are a partial cache recipe. Use these exact
references in the acceptance DaemonSets, and install matching Linux Calico
components and CRDs before testing the combined cluster.

This set is a candidate for HostProcess testing on both OS versions. The current
live harness remains on its recorded Calico 3.32.0 baseline. VM image baking,
fresh-clone cache reuse, and CNI acceptance for this new set remain pending.

## Promotion gates

- Require successful build and test jobs for the exact source and builder commits.
- Resolve and record registry digests before preparing VM images.
- Exercise fresh Server 2022/2025 workers with distro-matched CNI and kube-proxy.
- Verify Service traffic, deny/allow policy, MTU repair, and continuous survivors.
- Repeat CAPI removal/replacement and tunnel endpoint changes.
- Qualify IPv6 in a separate dual-stack VM fleet.

These branches are candidates. Portable tests and image publication are narrower
than the VM acceptance matrix. Production merging remains gated on that evidence.
