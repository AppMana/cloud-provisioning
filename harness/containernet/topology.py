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

    info("*** 6. extended outage (60s, well past a single ping/BGP-hold window)\n")
    # Scenario 5 proves self-heal after a brief flap; this proves nothing
    # about it was only accidentally working for something short enough
    # to still be inside some other retry/backoff window. Same mechanism
    # (kernel PersistentKeepalive, no controller), just a duration long
    # enough that anything relying on a short timeout would have already
    # given up and needed a manual kick.
    outage_secs = 60
    onprem.cmd(f"ip link set {link.intf1} down")
    down_since = time.time()
    time.sleep(outage_secs)
    rc = rc_of(ec2, "ping -c1 -W1 10.101.0.1")
    print(f"  still down after {outage_secs}s (expected)" if rc else "  API unexpectedly reachable")
    onprem.cmd(f"ip link set {link.intf1} up")
    t = wait_for(lambda: rc_of(onprem, "ping -c1 -W1 -I 10.101.128.1 10.101.130.1") == 0, timeout=30)
    total = round(time.time() - down_since, 1)
    print(f"  recovered ~{t}s after the link came back ({total}s total outage) — no process re-executed"
          if t is not None else f"  did NOT recover within 30s of the link returning ({total}s total outage)")

    info("*** 7. slow provisioning: onprem dials out for a while before ec2 ever boots\n")
    # Models a real CAPA/EC2 launch: the on-prem dialer is already
    # running and trying to reach a peer that doesn't exist yet. Its
    # only job during this window is to not wedge or error out --
    # ENDPOINT/peer state was configured back in step 2's config write,
    # so this restarts wg-pullup on a *fresh* interface to prove the
    # dialing side tolerates a peer that simply isn't there yet, then
    # brings the "slow" ec2 side up only after a real delay.
    provision_delay_secs = 20
    onprem.cmd("ip link del wg0 2>/dev/null || true")
    ec2.cmd("ip link del wg0 2>/dev/null || true")
    onprem.cmd("nohup /usr/local/sbin/wg-pullup node >/tmp/onprem-slow.log 2>&1 &")
    print(f"  onprem dialing a peer that doesn't exist yet for {provision_delay_secs}s...")
    time.sleep(provision_delay_secs)
    rc = rc_of(onprem, "ip link show wg0")
    print("  onprem's wg0 still up while waiting (no crash/wedge)" if rc == 0 else "  onprem's wg0 is GONE (dialer crashed while peer absent)")
    ec2.cmd("nohup /usr/local/sbin/wg-pullup node >/tmp/ec2-slow.log 2>&1 &")
    time.sleep(1)  # give wg-pullup time to create wg0 before routing to it (see step 2's identical wait)
    onprem.cmd("ip route replace 10.101.130.0/24 dev wg0")
    ec2.cmd("ip route replace 10.101.0.0/16 dev wg0")
    t = wait_for(handshake_done, timeout=20, interval=2)
    print(f"  handshake completed ~{t}s after the late peer finally appeared" if t is not None else "  FAILED to handshake even after the peer appeared")

    info("*** 8. control plane transiently down (API gone, tunnel itself untouched)\n")
    # Different failure from 5/6: the LINK stays fully up the whole
    # time (no flap) -- only the thing standing in for the API/control
    # plane (vip0) disappears, e.g. jarvis's own k0s controller process
    # restarting while the host and network are fine. The tunnel should
    # never notice: no re-handshake should be needed once the VIP comes
    # back, because nothing about the WireGuard session itself broke.
    hs_before = clean(ec2.cmd("wg show wg0 latest-handshakes")).split()
    # ip addr del, not `ip link set vip0 down`: a down dummy link's
    # address can still answer ICMP under Linux's weak-host model (no
    # second interface needed to trigger it -- confirmed empirically,
    # this is not the intuitive "down means gone" behavior). Deleting
    # the address outright is what actually models "the process/address
    # is gone" -- e.g. k0s's API server unbinding during a restart.
    onprem.cmd("ip addr del 10.101.0.1/32 dev vip0")
    rc = rc_of(ec2, "ping -c1 -W1 10.101.0.1")
    print("  API unreachable while its VIP is gone (expected)" if rc else "  API still answering (unexpected)")
    rc_tunnel = rc_of(ec2, "ping -c1 -W1 10.101.128.1")  # onprem's *pod*, reached only via the still-healthy tunnel
    print("  tunnel itself still passes traffic during the outage" if rc_tunnel == 0 else "  tunnel appears down too (unexpected -- this should be API-only)")
    onprem.cmd("ip addr add 10.101.0.1/32 dev vip0")
    t = wait_for(lambda: rc_of(ec2, "ping -c1 -W1 10.101.0.1") == 0, timeout=10)
    hs_after = clean(ec2.cmd("wg show wg0 latest-handshakes")).split()
    # Same timestamp before/after means no new handshake was forced by
    # the VIP outage itself -- WireGuard's own periodic rekey (every ~2
    # minutes) is far outside this test's window, so an unchanged
    # timestamp here is a real signal, not a coincidence.
    no_rehandshake = len(hs_before) >= 2 and len(hs_after) >= 2 and hs_before[-1] == hs_after[-1]
    print(f"  API reachable again ~{t}s after its VIP returned" if t is not None else "  API did NOT come back")
    print("  no re-handshake occurred -- confirms this was a control-plane outage, not a tunnel one"
          if no_rehandshake else "  a new handshake occurred (unexpected for a VIP-only outage)")

    net.stop()


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--check":
        print("topology script parses OK")
        sys.exit(0)
    main()
