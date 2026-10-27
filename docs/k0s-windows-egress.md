# k0s API egress with Windows workers

Mixed Windows/Linux k0s sites must validate API-server access to admission
webhooks separately from node readiness and Windows pod traffic.
k0s 1.36.2 uses Konnectivity for cluster egress. Its servers select agents
using `destHost,defaultRoute,default`; the native agents advertise their node IP.
A Service address does not match those node identities. Without a default-route
advertiser, the fallback can select a Windows agent that cannot reach the Service.

Kubernetes documents that [Windows hosts cannot access Service IPs](https://kubernetes.io/docs/tasks/debug/debug-cluster/windows/#network-troubleshooting). The [Konnectivity strategies](https://pkg.go.dev/sigs.k8s.io/apiserver-network-proxy/pkg/server/proxystrategies)
distinguish destination-host matching, explicit default-route agents and random
healthy backends. Service egress must select agents with native Service
reachability while preserving node-specific routing for Windows kubelets.

## Native controller fix

The [native k0s patch](../providers/k0s/README.md#native-linux-default-routes-for-api-egress)
adds Linux default-route agents through the distribution's existing manifest
owner and removes the server's random fallback. Existing mixed-OS node agents
keep their identities and remain responsible for explicit kubelet destinations.
Qualify a rebuilt controller with this patch; native runtime and upgrade
validation remain required. A harness-installed addon does not qualify the
controller fix.

Rollback must cover the generated manifest and its resources as well as the
controller executable and service arguments. Disabling Konnectivity does not
remove an existing manifest stack. On an isolated test site that originally
disabled it, restore the original controller first, then let its existing stack
reconciler apply an empty Konnectivity manifest and verify resource removal
before removing the empty directory. Deleting the directory while the
controller is stopped does not deliver a removal event to that reconciler.
Record service restoration and resource cleanup separately; cleanup performed
by the test driver does not prove autonomous resource rollback.

## Product guard

The controller refuses a Windows claim on a k0s site that routes API egress
through Konnectivity without the patched controller's
`konnectivity-agent-default-route-linux` agents, naming the patch. A site
without Konnectivity is unaffected: its API servers dial Services directly.

## Validation and failure boundaries

Readiness alone is insufficient. Verify each default-route agent has a
connection open to every controller. Exercise server dry-run CAPI Machine
updates through each API server, native exec into both Windows versions, and
actual provisioning/removal. Observe webhook sockets or dial logs to establish
which agent carries traffic. Preserve failures and the original pre-change
baseline.

When all default-route agents disappear, the server's `default` fallback can
again select an unsuitable Windows agent. The patch does not resolve Windows
VFP fragmentation or intermittent GPU process startup.
