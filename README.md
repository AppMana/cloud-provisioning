# cloud-provisioning

Work in progress utility for provisioning nodes to on-premises, firewalled
clusters.

## What it does

Joins a cloud VM (with a public IP) as a worker node to a k0s cluster that
sits behind NAT/CGNAT with no inbound connectivity — so the cluster can
place public-facing workloads (an ingress gateway, a VPN endpoint) on a
node that the internet can actually reach, while the control plane stays
private.

The node is a normal k0s worker. Pod networking is Calico's ordinary BGP
mesh (`overlay: Never`) carried over a WireGuard underlay — no VXLAN, no
overlay reconfiguration. The tunnel does the encapsulation; Calico does
not know a WAN is involved.

## How it works with k0s

1. **Provision** — Cluster API (CAPA `AWSMachine`, run against the k0s
   cluster as an externally-managed/standalone machine) creates the VM.
   The AWS specifics stay behind the CAPA seam; other infrastructure
   providers slot in the same way.
2. **Bootstrap** — a real k0s join token (`k0s token create --role=worker`
   on an existing node) plus a WireGuard listener config are rendered into
   cloud-init from a versioned template (`join-patterns/k0s-worker.cloud-config.tmpl`,
   via `controller/cmd/render-join-data`) and set directly as
   `Machine.spec.bootstrap.dataSecretName` — no bootstrap provider. k0smotron's
   own worker bootstrap controller was tried first and doesn't work here: it
   unconditionally requires `Cluster.spec.controlPlaneRef` to resolve to a real
   object, which no externally-managed cluster has (confirmed by reading its
   source, not guessed). That cloud-init brings up the WireGuard listener and
   gates the `k0s install worker` join behind "wait until the API VIP is
   reachable."
3. **Tunnel** — the cluster dials *out* to the VM's public IP (the VM has
   no inbound requirement on the cluster side). WireGuard needs only one
   reachable endpoint, and the VM's public IP is it.
   `PersistentKeepalive` on the cluster-side dialer is the entire
   self-heal mechanism: on any link flap the tunnel re-handshakes and
   BGP re-converges in ~1s, with no controller and no Kubernetes
   involvement — which is exactly why the tunnel is a static dialer, not
   a DaemonSet.
4. **Network** — once the tunnel is up, `k0s install worker` joins over
   it, Calico peers BGP across the wg0 link, and pod/service traffic
   flows. Drop the cluster's Calico MTU to ~1420 to fit under WireGuard
   overhead.

## Layout

```
harness/                reproducible two-netns test of the tunnel + BGP + self-heal
harness/containernet/   the same tunnel scripts under a Docker-based network
                         simulation, with a flappable link standing in for
                         the internet — see harness/containernet/README.md
harness/aws-bringup/    the same scripts against one real, billed EC2
                         instance and a real on-prem host, over the real
                         internet — see harness/aws-bringup/README.md
manifests/              CAPA worker templates (Machine + AWSMachine, no bootstrap
                         provider); the cluster-side WG dialer + endpoint controller
join-patterns/          versioned cloud-init templates, one per join mechanism --
                         rendered by controller/cmd/render-join-data, never hand-typed
controller/              Go module: the endpoint-watcher controller, the Go dialer
                         (netlink/wgctrl, no shelling out), and render-join-data
scripts/aws/            one-time IAM bootstrap for the least-privilege identity
                         harness/aws-bringup/ runs under
```

## Status

The design is validated at three levels of fidelity, each closer to the
real thing than the last:

- `harness/wg-wan-worker.sh` — two raw `ip netns`, a WireGuard tunnel, and
  bird BGP, run as root on the bare host.
- `harness/containernet/` — the actual boot-time scripts
  (`wg-pullup.sh`/`firstboot.sh`, the same files the systemd units in
  `harness/containernet/image/` run for real) driven inside two Docker
  containers linked by containernet, with real latency and a link that can
  be flapped on command.
- `harness/aws-bringup/` — the same `wg-pullup.sh`, unmodified, driving a
  real `t4g.micro` in EC2 and a real on-prem host (jarvis), dialing over
  the actual internet. This is the level that caught two bugs the
  simulations couldn't: Ubuntu's AppArmor profile for `/usr/bin/wg`
  confines it to `/etc/wireguard/**`, so a keyfile under the default
  `mktemp` (`/tmp`) location fails with a bare `fopen: Permission denied`
  — fixed in `wg-pullup.sh` itself, so every environment gets the fix.
  The other was in the test harness, not the design: `ping -I <device>`
  binds the socket to that device (forcing egress out a dummy interface
  with no real link), where `ping -I <address>` binds only the source
  address and lets normal routing pick `wg0` — the harness now does the
  latter.

All three independently confirm: ordering (no API access before the
tunnel is up), a successful WireGuard handshake by dialing out,
pod-to-pod connectivity riding the tunnel, MTU enforcement under
WireGuard overhead (1420), and self-heal after a link flap using only
WireGuard's `PersistentKeepalive` kernel timer — no controller, nothing
re-executed.

The remaining glue -- the cluster-side dialer's endpoint discovery -- is
now `controller/cmd/endpoint-controller`: it watches the provider-agnostic
`Machine.status.addresses` (never `AWSMachine` directly) and keeps the
dialer's peer Secret current, plus (optionally) an HTTPRoute's
`external-dns.alpha.kubernetes.io/target` annotation for a Gateway pinned
to the node via hostPorts. `controller/cmd/dialer` replaced the original
shell-script DaemonSet with direct netlink/wgctrl calls, avoiding both
shelling out and the AppArmor confinement `wg` runs under.

This design is now being deployed for real, end to end, against the
production `hilton` k0s cluster (a t4g.nano in ap-east-1) -- see
`join-patterns/` and `manifests/capi-direct-bootstrap/` for what that
deployment actually runs.
