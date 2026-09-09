# Focused VM routing regression

This legacy harness tests dialer routing and reboot behavior with two QEMU/KVM
VMs. Use the [Go single-NIC harness](../../docs/validation/single-nic-vms.md) for
current distribution, CAPI lifecycle, placement and outage matrices.

## Prerequisites and topology

The scripts require Docker, containerlab, KVM, Go, k0sctl, kubectl and SSH.
`topo.clab.yml` uses `vrnetlab/canonical_ubuntu:jammy`. Its launcher must support
`EXTRA_SETUP_PATH` and forward TCP 22/6443 and UDP 51820 into the guests.
The configured shared image checkout is `~/Documents/vrnetlab`; rebuild that
image when changing its launcher.

Each guest has one Ethernet interface behind QEMU user-mode NAT. Both wrappers
share the containerlab management bridge, with no additional topology links.
This harness uses SSH and forwarded ports for management; it does not provide
the serial QGA management and network isolation of the Go harness.

`onprem` runs a k0s controller/worker and the site dialer DaemonSet. `cloud` joins
as a k0s worker through k0sctl and runs both a bootstrap dialer service and a
cloud dialer DaemonSet. The worker carries the cloud-worker label and the
internet-facing taint. Version and installation settings are in `k0sctl.yaml`.
This setup exercises dialer coexistence and scheduling; it does not exercise
product token generation or CAPA provisioning.

## Run

From this directory:

```sh
scripts/build-dialers.sh
scripts/run-scenario.sh "10.244.0.0/16"
scripts/run-scenario.sh "0.0.0.0/0,::/0"
```

The build script compiles `bin/dialer-fixed` from the working tree. The scenario
script accepts one argument, the pod CIDR accept-list value. Both cases require
host routes to remain independent of broad WireGuard accept-list entries.

Each scenario creates fresh VMs, configures k0s and WireGuard, starts the dialers,
reboots `onprem`, and observes connectivity for up to ten minutes. It checks
that the cloud bootstrap service remains active alongside the cloud DaemonSet,
and that the site DaemonSet excludes the tainted cloud worker. Output is written
to `logs/`. Generated keys and runtime artifacts must remain out of Git.

## Optional Tailscale checks

`WGDIALER_TEST_TAILSCALE_AUTHKEY` supplies a disposable test auth key. When unset,
Tailscale assertions are reported as skipped. The guest joins under a per-run
hostname, and the workstation checks its tailnet visibility independently of
the guest's network.

`WGDIALER_TEST_TAILSCALE_HOST_PROFILE` selects the workstation's Tailscale profile
(default `b5e0`). The script switches the host profile for checks and restores
the original profile on exit. Use a scoped, revocable key and a matching profile;
keep auth keys out of repository files and shared logs.

## Interpreting failures

A route failure can remove guest reachability before reboot. The script detects
repeated API health failures while waiting for the dialer and records immediate
connectivity loss. In that case, later checks requiring guest access cannot run.

QEMU user-mode NAT can stall SSH or kubectl traffic under concurrent load, so
management calls use retries. Keep passthrough disabled for the configured image:
its static guest addressing does not implement that mode's addressing contract.

Tailscale's stale-peer transition is timing-sensitive. General network and route
checks establish the routing invariant; Tailscale visibility is supplementary.
These tests do not establish CAPI Machine association, cloud instance lifecycle,
or distribution-default CNI reachability across a remote-worker matrix.
