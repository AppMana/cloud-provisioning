# AWS Windows CNI host routes

Experimental extension for upstream `aws/amazon-vpc-cni-plugins` revision
`8c8909eabf08b598af67afb9e540fab67a1f4ff3`.

`host-routes.patch` adds optional `hostRouteCIDRs` to the vpc-bridge configuration.
On Windows, the existing endpoint creation path installs an HCN SDNRoute policy
with `NeedEncap` for each selected prefix, using the same mechanism as Service
routing. The CNI plugin retains endpoint creation and deletion ownership.
An omitted or empty list preserves upstream behavior.

Prefixes must be canonical unicast IPv4 networks, exclude local-scope and
multicast ranges, and not overlap any configured ENI subnet. Default routes are
rejected. Duplicate prefixes are collapsed. The host must independently have
working routes and forwarding for these destinations; this extension does not
establish a tunnel or authorize changing host networking.

Apply to a clean checkout using `./apply.sh /path/to/amazon-vpc-cni-plugins`.
Build with the upstream Windows target and `enablek8sconnector` tag. Use a
concrete Go toolchain version compatible with the upstream module declaration.
Run `go test ./plugins/vpc-bridge/config` to verify prefix admission.

The published image recipe still selects the upstream binary. Integrating a
patched binary requires pinned artifact provenance, native endpoint lifecycle
verification, source-preserving cross-site traffic, and unchanged VPC and
management connectivity on both supported Windows versions.

The Windows initializer accepts `hostRouteCIDRs` only with the default `vpc`
pod gateway, a running native pod-routing tunnel service, and a checksum-pinned
recipe listing `hostRouteCIDRs` in `features`. Every prefix must be covered by
an explicit SNAT exclusion and must not overlap VPC or Service ranges. The
initializer still refuses to replace an existing, different CNI configuration.

The image workflow accepts `--cni-recipe` alongside `--cni-archive` for a
candidate artifact. Revisions retain the previous recipe and require native
verification of the existing installation before replacing owned binaries.
Published recipes remain the default when no candidate recipe is supplied.
