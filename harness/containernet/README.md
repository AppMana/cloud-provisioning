# Containernet integration test

Runs the actual `wg-pullup`/`firstboot` scripts (the same files a real
node's systemd units execute at boot) inside two real Docker containers
linked by containernet, with the link's latency, and flappability,
under real control. This complements `../wg-wan-worker.sh`, which
proves the mechanism in raw network namespaces; this proves the
packaged artifact against a more realistic (if messier) environment.

## Setup

Requires containernet (a Mininet fork) and its `mnexec` helper, neither
of which are packaged for pip. `./setup.sh` automates this end to end
(idempotent, safe to re-run) -- clones containernet into `./containernet`
(gitignored: a large upstream checkout, not vendored), builds its venv,
`mnexec`, and the `wg-node:test` image:

```bash
./setup.sh
```

Or by hand, if you'd rather see each step:

```bash
git clone --depth 1 https://github.com/containernet/containernet.git
cd containernet
python3 -m venv venv && source venv/bin/activate
pip install -e . python-iptables
make mnexec && sudo cp mnexec /usr/local/bin/
sudo apt-get install -y openvswitch-switch   # mininet's default switch backend
cd .. && ./build-image.sh
```

Run (root; mininet manipulates network namespaces and iptables):

```bash
source containernet/venv/bin/activate
sudo -E env PATH=$PATH python3 -u topology.py
```

Expect these checks, in order:

1. **ordering** — ec2 cannot reach the API VIP before the tunnel exists
2. **the real boot script** establishing the tunnel (not a reimplementation)
3. **pod-to-pod** over the tunnel
4. **MTU enforcement** under WireGuard overhead
5. **self-heal after a brief link flap** — kernel `PersistentKeepalive`, no controller involved
6. **self-heal after an extended (60s) outage** — proves 5 wasn't only working because the flap was short enough to be inside some other retry window
7. **slow provisioning** — the on-prem side dials a peer that doesn't exist yet for 20s (standing in for a real CAPA/EC2 launch delay) and must neither crash nor wedge, then handshake normally once the peer finally appears
8. **control plane transiently down** — the API's own address disappears (e.g. a k0s controller restart) while the tunnel and network path stay fully up; the tunnel must not notice or need to re-handshake, since this is a different failure than 5/6 (that's a network path failure; this is a control-plane one, orthogonal to the tunnel's own health)

Scoping note: containernet drives the real `wg-pullup`/`firstboot`
scripts and real Linux network namespaces, so it can faithfully model
network- and timing-layer failures (5-8 above). It cannot model
failures in the CAPA/CAPI reconciliation loop itself (IAM permission
gaps, AWSMachine spec immutability, AWS quota errors, a Machine stuck
in `Provisioning`) -- that's a different layer entirely, validated
instead by exercising the real control loop against a real or `kind`
cluster.

## What this tool actually gives you (read before extending this test)

containernet's `Docker` node class is an **exec-driven filesystem +
network namespace sandbox**, not a booting VM. Confirmed by reading its
source: `privileged` is hardcoded `False`, the image's own
`ENTRYPOINT`/`CMD` is discarded, and every `node.cmd()` call goes
through a fresh `docker exec … bash`, independent of whatever PID 1 is.
A literal systemd-as-PID1 boot was attempted (`dcmd="/sbin/init"`,
`cap_add=[sys_admin]`, `--cgroupns=host`) and reliably exits immediately
— this is a real constraint of the tool on this host, not a
misconfiguration to keep chasing. The systemd unit files in `image/`
are the real deployment artifact for an actual cloud-init boot; this
test invokes the same shell scripts directly via `cmd()`.

This turned out not to matter for what we're actually validating:
WireGuard's `PersistentKeepalive` is a **kernel** timer. Once
`wg-pullup` has run once, the tunnel reconnects on its own after a link
flap with nothing running in userspace — so "no init system" doesn't
weaken the self-heal proof at all.

Three non-obvious fixes were needed to get a trustworthy result, kept
here so nobody re-derives them the hard way:

- **`Intf.ifconfig()` and `addLink(params1=…)` don't reliably reach a
  Docker node's netns.** Interfaces come up down, addresses don't take.
  Every network mutation in `topology.py` goes through
  `node.cmd("ip …")` instead — this is the one path proven to work.
- **Docker's default bridge network is a second, uncontrolled path**
  between the two containers that a mininet link flap can't touch —
  `network_mode="none"` is required or a self-heal test proves nothing.
- **Mininet's default link addressing is a `/8`**, which is a superset
  of any `10.x.x.x` range you might be testing (ours is
  `10.101.0.0/16`) — Linux's weak-host ARP model will happily answer
  for it over the raw link with no tunnel at all. Use a disjoint range
  (this test uses `203.0.113.0/24`, TEST-NET-3) for anything meant to
  simulate "the public internet."

And the bug that actually cost the most time here was in this
script, not containernet: `wait_for()` can legitimately return `0.0`
on a near-instant success, and `if t:` treats that as failure. Use
`if t is not None:`.
