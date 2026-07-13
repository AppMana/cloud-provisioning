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
2. **Bootstrap** — k0smotron's bootstrap provider (`K0sWorkerConfig`)
   mints the k0s join token and generates cloud-init. That cloud-init
   also brings up a WireGuard listener and gates the `k0s install worker`
   join behind "wait until the API VIP is reachable."
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
manifests/              CAPA + k0smotron worker templates; the cluster-side WG dialer
```

## Status

The design is validated twice, at two levels of fidelity:

- `harness/wg-wan-worker.sh` — two raw `ip netns`, a WireGuard tunnel, and
  bird BGP, run as root on the bare host.
- `harness/containernet/` — the actual boot-time scripts
  (`wg-pullup.sh`/`firstboot.sh`, the same files the systemd units in
  `harness/containernet/image/` run for real) driven inside two Docker
  containers linked by containernet, with real latency and a link that can
  be flapped on command.

Both independently confirm: ordering (no API access before the tunnel is
up), a successful WireGuard handshake by dialing out, pod-to-pod
connectivity riding the tunnel, MTU enforcement under WireGuard overhead
(1420), and self-heal after a link flap using only WireGuard's
`PersistentKeepalive` kernel timer — no controller, nothing re-executed.

The provisioning manifests are templates; the cluster-side dialer's
endpoint discovery from `AWSMachine.status` is the remaining glue.
