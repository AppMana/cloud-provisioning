# Containernet integration test

Runs the actual `wg-pullup`/`firstboot` scripts (the same files a real
node's systemd units execute at boot) inside two real Docker containers
linked by containernet, with the link's latency, and flappability,
under real control. This complements `../wg-wan-worker.sh`, which
proves the mechanism in raw network namespaces; this proves the
packaged artifact against a more realistic (if messier) environment.

## Setup

Requires containernet (a Mininet fork) and its `mnexec` helper, neither
of which are packaged for pip:

```bash
git clone --depth 1 https://github.com/containernet/containernet.git
cd containernet
python3 -m venv venv && source venv/bin/activate
pip install -e . python-iptables
make mnexec && sudo cp mnexec /usr/local/bin/
sudo apt-get install -y openvswitch-switch   # mininet's default switch backend
```

Build the node image once:

```bash
./build-image.sh
```

Run (root; mininet manipulates network namespaces and iptables):

```bash
source /path/to/containernet/venv/bin/activate
sudo -E env PATH=$PATH python3 -u topology.py
```

Expect six passing checks: ordering (no API access before the tunnel),
the real boot script establishing the tunnel, pod-to-pod over the
tunnel, MTU enforcement, and self-heal after a link flap with the
tunnel's kernel-level `PersistentKeepalive` — no controller involved.

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
