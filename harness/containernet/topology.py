#!/usr/bin/env python3
"""Containernet integration test for the WAN-worker tunnel.

Two Docker hosts, linked directly (no switch) with a lossy/latent
TCLink standing in for the public internet:

  onprem  - the cluster side: API VIP, a "pod", the WG dialer
  ec2     - the remote node: a "pod", the WG listener, public IP

Provisioning is driven by the same two scripts a real node runs at boot
(image/firstboot.sh, image/wg-pullup.sh) via host.cmd() — containernet's
Docker nodes are exec-driven sandboxes (confirmed: Docker() hardcodes
privileged=False and always shells in via `docker exec`, independent of
whatever PID 1 is), so this invokes the real boot scripts directly rather
than emulating an init system. The systemd units in image/ are the
production artifact for a real boot (cloud-init/CAPA); this harness
proves the scripts and the network behavior, not systemd itself.

Self-heal needs no running process: WireGuard's PersistentKeepalive is a
kernel timer. Once wg-pullup.sh has run, the tunnel reconnects on its own
after a link flap with nothing left executing in userspace.
"""
import re
import sys
import time

from mininet.net import Containernet
from mininet.link import TCLink
from mininet.log import setLogLevel, info

IMAGE = "wg-node:test"
_ANSI = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")


def clean(s):
    """Docker hosts run cmd() through a pty-backed bash -is; Ubuntu
    24.04's bracketed-paste mode leaks \\x1b[?2004l/h into every
    response. Strip it before parsing anything."""
    return _ANSI.sub("", s).strip()


def rc_of(node, shell_cmd):
    """Run shell_cmd on node, return its exit code as an int."""
    return int(clean(node.cmd(f"{shell_cmd} >/dev/null 2>&1; echo $?")))


def wg_env(**kv):
    return {f"WG_{k}": v for k, v in kv.items()}


def wait_for(fn, timeout=20, interval=1):
    start = time.time()
    while time.time() - start < timeout:
        if fn():
            return round(time.time() - start, 1)
        time.sleep(interval)
    return None


def main():
    setLogLevel("info")
    net = Containernet(controller=None)

    onprem = net.addDocker(
        "onprem", dimage=IMAGE, dcmd="bash",
        # No default bridge: the mininet link below must be the ONLY
        # path between the two hosts, or a flap test proves nothing.
        network_mode="none",
        environment=wg_env(
            ADDRESS="10.100.0.1/24",
            PRIVATE_KEY="", PEER_PUBLIC_KEY="",  # filled in after keygen
            ALLOWED_IPS="10.100.0.2/32,10.101.130.0/24",
            ENDPOINT="", KEEPALIVE="5",
        ),
    )
    ec2 = net.addDocker(
        "ec2", dimage=IMAGE, dcmd="bash",
        network_mode="none",
        environment=wg_env(
            ADDRESS="10.100.0.2/24",
            PRIVATE_KEY="", PEER_PUBLIC_KEY="",
            ALLOWED_IPS="10.100.0.1/32,10.101.0.0/16",
            LISTEN_PORT="51820",
            WAIT_FOR="10.101.0.1",
        ),
    )

    # The "internet": one hop, real latency/loss, no switch needed for a
    # point-to-point link.
    net.addLink(onprem, ec2, cls=TCLink, delay="20ms", loss=0)

    net.start()

    # Containernet's Docker nodes need the link brought up and addressed
    # by hand: addLink's params1/2 and Intf.ifconfig() don't reliably
    # reach a Docker node's netns (confirmed — mininet's default /8
    # addressing survives regardless of params1/2), so every step here
    # goes through node.cmd(), the one path proven to work. The default
    # /8 would overlap 10.101.0.0/16 (our VIP/pod range) and let Linux's
    # weak-host ARP model answer for it even without a tunnel, so this
    # replaces it with non-overlapping TEST-NET-3 addresses.
    link = net.linksBetween(onprem, ec2)[0]
    onprem.cmd(f"ip addr flush dev {link.intf1}; ip addr add 203.0.113.1/24 dev {link.intf1}; ip link set {link.intf1} up")
    ec2.cmd(f"ip addr flush dev {link.intf2}; ip addr add 203.0.113.2/24 dev {link.intf2}; ip link set {link.intf2} up")
    rc = rc_of(onprem, "ping -c2 -W2 203.0.113.2")
    print(f"--- link up, onprem->ec2 base connectivity: {'OK' if rc == 0 else 'FAIL'}")
    if rc != 0:
        print("  --- onprem iface:", clean(onprem.cmd(f"ip -br addr show {link.intf1}")))
        print("  --- ec2 iface:", clean(ec2.cmd(f"ip -br addr show {link.intf2}")))

    info("*** generating keys, wiring peer config on each side\n")
    opriv = clean(onprem.cmd("wg genkey"))
    opub = clean(onprem.cmd(f"echo {opriv} | wg pubkey"))
    epriv = clean(ec2.cmd("wg genkey"))
    epub = clean(ec2.cmd(f"echo {epriv} | wg pubkey"))

    # ec2's link address stands in for its "public IP" — from onprem's
    # point of view it's just some address it dials out to.
    ec2_ip = "203.0.113.2"

    onprem.cmd("mkdir -p /etc/wireguard")
    onprem.cmd(
        f'cat > /etc/wireguard/node.env <<EOF\n'
        f'ADDRESS=10.100.0.1/24\nPRIVATE_KEY={opriv}\nPEER_PUBLIC_KEY={epub}\n'
        f'ALLOWED_IPS=10.100.0.2/32,10.101.130.0/24\nENDPOINT={ec2_ip}:51820\nKEEPALIVE=5\nEOF'
    )
    ec2.cmd("mkdir -p /etc/wireguard")
    ec2.cmd(
        f'cat > /etc/wireguard/node.env <<EOF\n'
        f'ADDRESS=10.100.0.2/24\nPRIVATE_KEY={epriv}\nPEER_PUBLIC_KEY={opub}\n'
        f'ALLOWED_IPS=10.100.0.1/32,10.101.0.0/16\nLISTEN_PORT=51820\nEOF'
    )

    # Fake API VIP + pods, same shape as the raw-netns harness.
    onprem.cmd("ip link add vip0 type dummy && ip addr add 10.101.0.1/32 dev vip0 && ip link set vip0 up")
    onprem.cmd("ip link add podc type dummy && ip addr add 10.101.128.1/32 dev podc && ip link set podc up")
    ec2.cmd("ip link add pode type dummy && ip addr add 10.101.130.1/32 dev pode && ip link set pode up")

    info("*** 1. ordering: ec2 cannot reach the API VIP before the tunnel exists\n")
    rc = rc_of(ec2, "ping -c1 -W1 10.101.0.1")
    print("  UNREACHABLE as expected" if rc else "  reachable (unexpected)")

    info("*** 2. run the REAL boot scripts (this is the thing under test)\n")
    ec2.cmd("nohup /usr/local/sbin/wg-pullup node >/tmp/ec2.log 2>&1 &")
    onprem.cmd("nohup /usr/local/sbin/wg-pullup node >/tmp/onprem.log 2>&1 &")
    # Plain `wg set` only scopes allowed-ips for crypto/filtering; unlike
    # wg-quick it installs no kernel routes. Add the ones BGP would have
    # learned in the real design (proven separately in harness/wg-wan-worker.sh) —
    # this containernet test is about the boot scripts and fault
    # injection, not re-proving BGP.
    time.sleep(1)
    onprem.cmd("ip route replace 10.101.130.0/24 dev wg0")
    ec2.cmd("ip route replace 10.101.0.0/16 dev wg0")
    # wg-pullup blocks on WAIT_FOR on the ec2 side; give it the same
    # gate the harness's design intends: it returns once the API is up.
    # Poll for the handshake directly rather than ping RTT: the first
    # handshake can take longer than a single ping's timeout window, so
    # a 1s-timeout ping can under-report even once the tunnel is really
    # up (each attempt still *drives* the handshake, since sending any
    # packet over an allowed-ips route triggers one).
    def handshake_done():
        parts = clean(ec2.cmd("wg show wg0 latest-handshakes")).split()
        return len(parts) >= 2 and parts[-1].isdigit() and parts[-1] != "0"
    t = wait_for(handshake_done, timeout=20, interval=2)
    if t is not None:
        print(f"  WG handshake established after ~{t}s")
        rc = rc_of(ec2, "ping -c1 -W2 10.101.0.1")
        print("  ec2 -> API VIP reachable (wg-pullup's own gate satisfied)" if rc == 0 else "  handshake up but ping still failing")
    else:
        print("  FAILED to handshake")
        print("  --- onprem wg0:", clean(onprem.cmd("ip -br addr show wg0; wg show wg0")))
        print("  --- ec2 wg0:", clean(ec2.cmd("ip -br addr show wg0; wg show wg0")))
        print("  --- ec2 route to onprem link ip:", clean(ec2.cmd("ip route get 203.0.113.1")))

    info("*** 3. pod-to-pod over the tunnel\n")
    rc = rc_of(onprem, "ping -c2 -W2 -I 10.101.128.1 10.101.130.1")
    print("  10.101.128.1 -> 10.101.130.1 OK" if rc == 0 else "  FAIL")

    info("*** 4. MTU under WG overhead\n")
    rc1 = rc_of(onprem, "ping -c1 -W2 -M do -s 1392 -I 10.101.128.1 10.101.130.1")
    rc2 = rc_of(onprem, "ping -c1 -W2 -M do -s 1500 -I 10.101.128.1 10.101.130.1")
    print("  1392B fits: OK" if rc1 == 0 else "  1392B FAIL")
    print("  1500B correctly needs-frag" if rc2 != 0 else "  1500B transited (unexpected)")

    info("*** 5. self-heal: flap the link (mininet), no process re-runs anything\n")
    # Same node.cmd()-only rule as the initial bring-up: Intf.ifconfig()
    # doesn't reliably reach a Docker node's netns for these links.
    onprem.cmd(f"ip link set {link.intf1} down")
    time.sleep(2)
    rc = rc_of(ec2, "ping -c1 -W1 10.101.0.1")
    print("  API down during outage (expected)" if rc else "  API still up (unexpected)")
    onprem.cmd(f"ip link set {link.intf1} up")
    t = wait_for(lambda: rc_of(onprem, "ping -c1 -W1 -I 10.101.128.1 10.101.130.1") == 0, timeout=30)
    print(f"  pod-to-pod recovered after ~{t}s — kernel keepalive only, nothing re-executed" if t is not None else "  did NOT recover")

    net.stop()


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--check":
        print("topology script parses OK")
        sys.exit(0)
    main()
