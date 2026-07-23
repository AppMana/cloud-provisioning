# VM single-NIC regression test (Level 4)

Reproduces the jarvis wg-dialer incident end to end: a real, single-NIC VM
boots real k0s (via `k0sctl`, the same tool that provisions jarvis), a
`wg-dialer` DaemonSet runs one of two real dialer binaries built from real
git revisions, and the harness asserts what happens to routing, general
internet, and (optionally) real Tailscale connectivity.

This exists because `../containernet/` — the next level down — cannot
reproduce this specific bug class: its `Docker` node class hardcodes
`privileged: False`, so it cannot do a genuine systemd/PID1 boot, and the
actual incident mechanism is a **boot-time race** between kubelet
resurrecting a stale DaemonSet pod (with old, bad `AllowedIPs` baked into
its args) and anything else having a chance to intervene. Reproducing that
race needs a real init system and a real kubelet, which needs a real VM.

## Why containerlab + the existing `vrnetlab` checkout, not raw Vagrant/libvirt or GNS3

- **containernet** is ruled out for this bug class (see above) — no dispute
  there, already the working assumption going in.
- **GNS3** is the most popular GUI-first network emulator, but it's built for
  interactive topology design, not git-diffable, CLI/CI-automatable
  topologies — a weaker fit for a repo-committed regression test.
- **[containerlab](https://containerlab.dev)** (already installed on this
  machine, `v0.75.0`) gives declarative, version-controlled YAML topologies
  and a scriptable CLI, and — critically — this machine already has a working
  `vrnetlab` checkout (`~/Documents/vrnetlab`, a `srl-labs/vrnetlab` fork) with
  an already-built `vrnetlab/canonical_ubuntu:jammy` image: a real Ubuntu
  22.04 cloud image booted under actual QEMU/KVM, wrapped so containerlab
  manages it like any other lab node. Reusing this — rather than standing up
  a fresh Vagrant+libvirt VM — means genuine systemd/PID1/kubelet boot
  behavior with zero new image-build step.
- Two sibling repos on this machine (`network-tests-vyos`,
  `libnccl-net-rdmaroute`) already demonstrate this exact pattern working —
  this harness's topology and driver script are modeled directly on theirs,
  not invented fresh.

## Topology: two nodes, each genuinely single-NIC, not "single NIC after a netplan workaround"

`topo.clab.yml` defines **two nodes, `onprem` and `cloud`, neither with any
`links:` entries at all**. Each node's only interface is containerlab's own
mgmt NIC (Docker-bridge-NATed, real internet egress) — there is no second NIC
to suppress via a netplan `dhcp4-overrides` workaround (an ad hoc VM
reproduction earlier needed exactly that fix, because a Vagrant+libvirt setup
had a second NAT-network NIC by default). Here there's simply nothing to
suppress: the mgmt NIC *is* the one real uplink on both nodes, exactly like
jarvis and its real EC2 peer. `wg0` (the WireGuard interface) is created
inside each guest by its own dialer process, dialing/listening over this
same NIC — there's no second link for it to "not conflict with"; the whole
point of this bug is that the hijack route competes with the real default
route **on the same link**.

- `onprem` mirrors the real on-prem side: real k0s (via `k0sctl`, config in
  `k0sctl.yaml`), the `wg-dialer` DaemonSet under test, dials out to `cloud`.
- `cloud` mirrors the real EC2/cloud-worker side, including joining k0s as a
  real worker Node (this was wrong in an earlier draft of this harness,
  which assumed `cloud` never runs k8s at all — the real
  `join-patterns/k0s-worker.cloud-config.tmpl` join flow always has it join).
  It's registered with the exact same node-label and taint the real join
  flow's kubelet args apply
  (`cloud-provisioning.appmana.com/role=cloud-worker`,
  `cloud-provisioning.appmana.com/internet-facing:NoSchedule` — see
  `controller/cmd/endpoint-controller/main.go`'s `cloudWorkerRoleLabel`/
  `cloudWorkerTaintKey` constants), via `k0sctl.yaml`'s `installFlags`. Two
  dialer processes end up running on `cloud`, deliberately, forever:
  - the bootstrap `wg-dialer.service` systemd unit, up before k0s ever joins
    (fed a static `/etc/wg-dialer/peers.json` — the real replacement for the
    old, unprotected `wg-quick` setup), and
  - once `cloud` is a real Node, a second, containerized copy scheduled by
    `manifests/wg-dialer-cloud-daemonset.tmpl.yaml` (mirroring
    `endpoint-controller`'s `ensureCloudDialerDaemonSet`), reading the exact
    same peers.json file.

  These two never hand off from one to the other — see
  `ensureCloudDialerDaemonSet`'s doc comment in
  `controller/cmd/endpoint-controller/main.go` for why running both forever
  is the deliberate safety property (the bootstrap tunnel must never depend
  on a later mechanism successfully taking over from it), not an oversight.
  The harness asserts this directly: `wg-dialer.service` must still report
  `active` on `cloud` *after* the DaemonSet pod starts there, and the
  on-prem DaemonSet's own pod must never schedule onto `cloud` at all (the
  taint/toleration exclusion, checked by node name).

There are no `links:` between them; containerlab's shared mgmt bridge stands
in for "the internet" — topologically, two independently NATed endpoints
reaching each other over *some* path, not a LAN shortcut. Real bidirectional
WireGuard peering rides over that same NATed mgmt link, exactly as it would
over the real internet between an on-prem node and EC2.

## The `EXTRA_SETUP_PATH` mechanism

`~/Documents/vrnetlab` is a shared checkout used by other repos too
(`libnccl-net-rdmaroute` already patches it for an RDMA-specific setup hook,
`RXE_SETUP_PATH`). This harness adds a second, generic hook,
`EXTRA_SETUP_PATH` (`/extra-setup.sh`), committed to the vrnetlab fork
alongside the existing RDMA one rather than replacing it — additive, so
existing consumers are unaffected. `cfg/onprem-setup.sh.tmpl` and
`cfg/cloud-setup.sh.tmpl` are each bind-mounted there for their respective
node (after `scripts/render-setup-script.sh` renders both from one shared,
freshly-generated SSH keypair — never committed) and run once via cloud-init
on first boot, installing `curl` and this harness's own SSH key into
`authorized_keys` so the driver script and (on `onprem` only) `k0sctl` can
reach the node.

The shared image's mgmt-NIC hostfwd port list (`mgmt_tcp_ports` /
`mgmt_udp_ports` in `common/vrnetlab.py` and
`ubuntu/docker/launch.py`) had to be extended twice for this harness: TCP/6443
(the k8s API, for `k0sctl`/`kubectl`) and UDP/51820 (real WireGuard peering
between `onprem` and `cloud` — without this, neither node's mgmt NAT would
ever forward a WireGuard handshake to the other).

If you change `~/Documents/vrnetlab/ubuntu/docker/launch.py`, rebuild the
shared image before re-running this harness: `cd ~/Documents/vrnetlab/ubuntu
&& make build`.

## Running a scenario

```bash
scripts/build-dialers.sh   # builds bin/dialer-old-vulnerable, bin/dialer-fixed
                           # from real git revisions (fb01961~1 and HEAD)

# RED -- old-vulnerable binary's own frozen --allowed-ips flag, must fail
# (proves the test actually tests something), against the REAL cloud peer
scripts/run-scenario.sh old-vulnerable "0.0.0.0/0,::/0" yes

# GREEN -- fixed binary, real pod-CIDR value fed to --pod-cidrs
scripts/run-scenario.sh fixed "10.244.0.0/16" no

# GREEN -- fixed binary, but --pod-cidrs itself set to the maximally toxic
# value. Must STILL stay GREEN: proves the kernel route the fixed binary
# installs is structurally independent of this input now (always just the
# peer's own tunnel address), not merely narrower by convention.
scripts/run-scenario.sh fixed "0.0.0.0/0,::/0" no
```

The second argument means different things depending on the first: for
`old-vulnerable` it's fed straight to that binary's own frozen `--allowed-ips`
flag (the historical, single mechanism that conflated "accept" and
"install-a-route"); for `fixed` it's fed to `--pod-cidrs` (WireGuard's own
accept-list only — see `controller/cmd/dialer/main.go`'s package doc for why
that's now a completely separate mechanism from the kernel route).

Each run deploys **both** VMs fresh, installs real k0s via `k0sctl` on
`onprem` only (config in `k0sctl.yaml`, mirroring jarvis's actual
`cluster/k0sctl.yaml`: k0s `1.36.2+k0s.0`, `role: controller+worker`,
`noTaints: true`), generates a **real, fresh WireGuard keypair for each
side**, deploys the chosen dialer binary as a real DaemonSet (`manifests/`)
on `onprem` peered against `cloud`'s real, reachable dialer-as-systemd-unit
listener (never a fake/unreachable peer), then reboots `onprem` and observes
for up to 10 minutes — this is slow by design; the boot race this bug
depends on needs a real reboot, not a warm-system route injection, and
multi-minute observation windows to catch it reliably. `cloud` is never
rebooted; it only needs to stay up as a stable listener throughout.

## The real Tailscale connectivity assertion — env-gated, never committed

The most important assertion this harness makes — that Tailscale itself
never goes stale/offline, not just "ping to 8.8.8.8 fails" — needs a real
Tailscale auth key to join `onprem` to a real tailnet under a disposable,
per-run hostname, and the assertion itself is checked from **this
workstation's own Tailscale view of that guest**, not from inside the guest.
That's deliberate: the real jarvis incident was that jarvis itself went
stale *to everyone else on the tailnet* — the guest's own,
possibly-compromised network stack is exactly the thing under test, so it
can't be the vantage point the check trusts. Checking from an independent
peer also means the assertion still works even when `onprem` has gone
completely unreachable (the "hijack takes out reachability immediately"
case below) — there's nothing to SSH into to run the check from.

Environment variables, both **optional**:

- `WGDIALER_TEST_TAILSCALE_AUTHKEY` — a real Tailscale auth key, used to join
  `onprem` under the hostname `wgdialer-harness-<pid>`. **Never commit this
  or write it into any file inside this repo.** If unset, every
  Tailscale-specific assertion is skipped, clearly logged as `SKIPPED`, not
  silently passed — so the harness still works for anyone without the key,
  and CI (which won't have the secret) degrades gracefully.
- `WGDIALER_TEST_TAILSCALE_HOST_PROFILE` — the `tailscale switch` profile
  name on *this workstation* whose tailnet the guest joins (default `b5e0`).
  The script switches to it before every check and restores whatever profile
  was active beforehand on exit, so it's safe to run alongside other
  Tailscale usage on this machine.

To run this for real:

```bash
export WGDIALER_TEST_TAILSCALE_AUTHKEY="tskey-auth-..."
scripts/run-scenario.sh old-vulnerable "0.0.0.0/0,::/0" yes
```

Anyone else running this harness needs their own scoped, revocable
Tailscale auth key for whatever tailnet they want to test against, and a
`tailscale switch` profile on their own machine already logged into it.

## The hijack can take out the guest's reachability immediately, not just after reboot

On this single-NIC topology, the toxic `AllowedIPs=0.0.0.0/0,::/0` hijack
doesn't need a reboot to manifest at all: once the old vulnerable dialer
pod starts on `onprem`, it can take out *every* path to that guest within
seconds -- `cloud` and this workstation are unaffected, since the hijack is
entirely local to `onprem`'s own routing table --
kubectl, SSH, even a raw TCP connect from the wrapper container straight to
the guest's own internal IP all go dark simultaneously, since there's no
second interface for anything to fall back on. This was confirmed directly:
while a run was investigated mid-flight, `docker exec <container> bash -c
'echo > /dev/tcp/10.0.0.15/22'` (from *inside* the vrnetlab wrapper,
bypassing the external qemu-hostfwd NAT entirely) still failed, while the
wrapper container's own network namespace was completely unaffected --
pinpointing the guest's own routing as what broke, not the harness's
plumbing around it.

`scripts/run-scenario.sh` accounts for this: while waiting for the dialer
pod to report `Running`, it also probes a lightweight `kubectl get --raw
/healthz`. If that fails **6 consecutive times** (~60s), it treats the
guest as having gone unreachable immediately rather than continuing to
wait out the full timeout — and records that directly as `HIJACK_SEEN=yes`
without needing to SSH in and literally `grep` for the `wg0` route (which
would itself require the reachability the bug just removed). The
baseline/Tailscale-join/reboot/observation steps are skipped in that case
since there's nothing reachable to run them against. This is a *stronger*
form of the same failure the reboot-based observation window is designed
to catch, not a different bug — GREEN scenarios must never hit this path.

## Known gotcha: QEMU's mgmt-network NAT (slirp) stalls under concurrent load

The default (non-passthrough) mgmt networking mode is QEMU's usermode
`slirp` NAT, hostfwd'ing a fixed port list (22, 6443, a handful of vendor
gnmi/gnoi ports — see `vrnetlab/ubuntu/docker/launch.py`) into the guest.
`slirp` is a known-slow, effectively single-threaded userspace network
stack: a raw TCP connect through it succeeds instantly even under load, but
the actual data exchange can stall for 30s+ when something else (a
concurrent container image pull, in particular) is sharing the link — this
is not a sign the VM or k0s is broken, just `slirp` being `slirp`.
`scripts/run-scenario.sh`'s `retry()` helper wraps every SSH/kubectl call
that talks to the VM for exactly this reason — a bare command hitting one
of these stalls under `set -e` would otherwise kill the whole run. If you
see a run die with a raw `kubectl`/`ssh` error rather than a real
assertion failure, that's this, not a new bug — wrap the failing call in
`retry` rather than debugging the VM itself.

(`CLAB_MGMT_PASSTHROUGH=true` was tried as a fix — it doesn't help here:
this image's `launch.py` always writes a hardcoded static guest network
config regardless of passthrough mode, so passthrough's "guest shares the
container's own IP" assumption doesn't hold, and it broke SSH entirely
during testing. Left disabled.)

## Known gotcha: the exact "Tailscale goes stale" symptom is not perfectly reliable

An earlier, ad hoc reproduction attempt (before this harness existed) found
that "general internet dies" reproduces solidly and immediately (within
~60-90s of boot), but the specific "Tailscale peer reports
`offline, last seen Xm ago`" transition took a genuinely single-NIC topology
*and* a boot-time race (the hijack installing while `tailscaled` is starting
cold, not against an already-stable connection) to reproduce even once, and
did not reproduce on every attempt even with that setup. If a RED run shows
`hijack_seen=yes` but `tailscale_went_stale=no`, that's a known-flaky
symptom, not evidence the harness itself is broken — the general-internet
assertion is the reliable one; treat the Tailscale-staleness assertion as
corroborating evidence, not the sole pass/fail gate, until it's shown to be
reliably reproducible across many runs.

## What this level does and doesn't cover

Covers: the actual boot-time mechanism of the jarvis incident (stale
DaemonSet pod resurrection racing ahead of any reconcile-based fix), real
k0s/kubelet, real routing, real (env-gated) Tailscale.

Doesn't cover: CAPI/CAPA reconciliation (a different layer, see
`../containernet/README.md`'s own scoping note), and doesn't touch a real
billed cloud resource the way `../aws-bringup/` does.
