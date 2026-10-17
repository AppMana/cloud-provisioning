package main

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// The remote endpoint keeps its original WireGuard UDP socket in the isolated
// transport namespace, but its workload sockets live in a separate namespace.
// Echoes therefore cannot bypass encryption through a local-address route.
func verifyNativeReceiveTrafficRecovery(t *testing.T, devices receiverDevices, cfg config, receiver, client string, restore func() error) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	parent, err := netns.Get()
	must(err)
	defer parent.Close()
	child, err := netns.New()
	must(err)
	must(netns.Set(parent))
	defer child.Close()
	link, err := netlink.LinkByName(client)
	must(err)
	must(netlink.LinkSetNsFd(link, int(child)))
	must(ensureRouteRule(cfg.routeTable, cfg.fwmark))
	type lane struct {
		family, source, target string
		conn                   *net.UDPConn
		attempts, failures     int
	}
	lanes := []lane{{family: "udp4", source: "10.90.0.2", target: "172.29.0.1"}, {family: "udp6", source: "fd90::2", target: "fd29::1"}}
	for i := range lanes {
		lane := &lanes[i]
		listener, err := net.ListenUDP(lane.family, &net.UDPAddr{IP: net.ParseIP(lane.target), Port: 17778})
		must(err)
		defer listener.Close()
		go func() {
			buffer := make([]byte, 128)
			for {
				n, remote, err := listener.ReadFromUDP(buffer)
				if err != nil {
					return
				}
				// Deliberate route loss may make a reply fail. Keep serving;
				// the client records the unanswered exchange as a failure.
				_, _ = listener.WriteToUDP(buffer[:n], remote)
			}
		}()
		inNamespace(t, child, parent, func() {
			lo, err := netlink.LinkByName("lo")
			must(err)
			must(netlink.LinkSetUp(lo))
			suffix := "/128"
			if lane.family == "udp4" {
				suffix = "/32"
			}
			addAddrs(t, lo, lane.source+suffix)
			live, err := netlink.LinkByName(client)
			must(err)
			must(netlink.LinkSetUp(live))
			_, destination, err := net.ParseCIDR(lane.target + suffix)
			must(err)
			must(netlink.RouteAdd(&netlink.Route{LinkIndex: live.Attrs().Index, Dst: destination, Scope: netlink.SCOPE_LINK}))
			must(setSysctl("ipv4/conf/all/rp_filter", "0"))
			must(setSysctl("ipv4/conf/"+client+"/rp_filter", "0"))
			lane.conn, err = net.DialUDP(lane.family, &net.UDPAddr{IP: net.ParseIP(lane.source)}, &net.UDPAddr{IP: net.ParseIP(lane.target), Port: 17778})
			must(err)
		})
		defer lane.conn.Close()
	}
	exchange := func(l *lane) error {
		l.attempts++
		payload := []byte(fmt.Sprintf("%s/%d", l.family, l.attempts))
		if err := l.conn.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			return err
		}
		if _, err := l.conn.Write(payload); err != nil {
			return err
		}
		buffer := make([]byte, 128)
		for {
			n, err := l.conn.Read(buffer)
			if err != nil {
				return err
			}
			if bytes.Equal(buffer[:n], payload) {
				return nil
			}
		}
	}
	for i := 0; i < 10; i++ {
		for j := range lanes {
			must(exchange(&lanes[j]))
		}
	}
	before, err := devices.Device(receiver)
	must(err)
	if len(before.Peers) != 1 || before.Peers[0].ReceiveBytes == 0 {
		t.Fatal("baseline lacks encrypted receive traffic")
	}
	lost, err := netlink.LinkByName(receiver)
	must(err)
	started := time.Now()
	must(netlink.LinkDel(lost))
	deadline := started.Add(60 * time.Second)
	consecutive := 0
	var firstSuccess time.Duration
	for {
		restoreErr := restore()
		healthy := restoreErr == nil
		for j := range lanes {
			if err := exchange(&lanes[j]); err != nil {
				lanes[j].failures++
				healthy = false
			}
		}
		if healthy {
			if consecutive == 0 {
				firstSuccess = time.Since(started)
			}
			consecutive++
			if consecutive == 10 {
				break
			}
		} else {
			consecutive = 0
		}
		if time.Now().After(deadline) {
			t.Fatalf("dual-stack workload recovery exceeded 60s: restore=%v lanes=%+v", restoreErr, lanes)
		}
		time.Sleep(100 * time.Millisecond)
	}
	after, err := devices.Device(receiver)
	must(err)
	if len(after.Peers) != 1 || after.Peers[0].ReceiveBytes == 0 || after.Peers[0].LastHandshakeTime.Before(started.Truncate(time.Second)) {
		t.Fatal("recovered workload lacks new encrypted transport evidence")
	}
	t.Logf("first sustained dual-stack recovery=%s; completed=%s", firstSuccess, time.Since(started))
	for _, lane := range lanes {
		t.Logf("%s attempts=%d failures=%d", lane.family, lane.attempts, lane.failures)
	}
}
