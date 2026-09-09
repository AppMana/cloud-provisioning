// Native experiment helper; run only in dedicated cldt-hov-* namespaces in VMs.
// setup creates only its named WireGuard devices in the host namespace, then
// moves them into the experiment namespace. It never moves a physical NIC.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type plan struct {
	Namespace, Device, Key, PeerKey, Endpoint string
	Port                                      int
	Create                                    bool
	Addresses, Allowed                        []string
}

func setup(p plan) error {
	if !strings.HasPrefix(p.Namespace, "cldt-hov-") || filepath.Base(p.Namespace) != p.Namespace || (p.Device != "cldt-hov-a" && p.Device != "cldt-hov-b") {
		return fmt.Errorf("invalid experiment scope")
	}
	key, e := wgtypes.ParseKey(p.Key)
	if e != nil {
		return fmt.Errorf("invalid private key")
	}
	peer, e := wgtypes.ParseKey(p.PeerKey)
	if e != nil {
		return fmt.Errorf("invalid peer key")
	}
	endpoint, e := net.ResolveUDPAddr("udp", p.Endpoint)
	if e != nil {
		return e
	}
	var allowed []net.IPNet
	for _, s := range p.Allowed {
		_, ip, e := net.ParseCIDR(s)
		if e != nil {
			return e
		}
		allowed = append(allowed, *ip)
	}
	var addresses []*netlink.Addr
	for _, s := range p.Addresses {
		a, e := netlink.ParseAddr(s)
		if e != nil {
			return e
		}
		a.Flags = 0x02
		addresses = append(addresses, a)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	host, e := netns.Get()
	if e != nil {
		return e
	}
	defer host.Close()
	defer netns.Set(host)
	var ns netns.NsHandle
	if p.Create {
		ns, e = netns.NewNamed(p.Namespace)
		if e != nil {
			return e
		}
		if e = netns.Set(host); e != nil {
			return e
		}
	} else {
		ns, e = netns.GetFromName(p.Namespace)
		if e != nil {
			return e
		}
	}
	defer ns.Close()
	link := &netlink.GenericLink{LinkAttrs: netlink.LinkAttrs{Name: p.Device}, LinkType: "wireguard"}
	if e = netlink.LinkAdd(link); e != nil {
		return e
	}
	moved := false
	defer func() {
		if !moved {
			netlink.LinkDel(link)
		}
	}()
	if e = netlink.LinkSetNsFd(link, int(ns)); e != nil {
		return e
	}
	moved = true
	if e = netns.Set(ns); e != nil {
		return e
	}
	client, e := wgctrl.New()
	if e != nil {
		return e
	}
	defer client.Close()
	if e = client.ConfigureDevice(p.Device, wgtypes.Config{PrivateKey: &key, ListenPort: &p.Port, Peers: []wgtypes.PeerConfig{{PublicKey: peer, Endpoint: endpoint, ReplaceAllowedIPs: true, AllowedIPs: allowed}}}); e != nil {
		return e
	}
	lo, e := netlink.LinkByName("lo")
	if e != nil {
		return e
	}
	if e = netlink.LinkSetUp(lo); e != nil {
		return e
	}
	for _, a := range addresses {
		if e = netlink.AddrReplace(lo, a); e != nil {
			return e
		}
	}
	link2, e := netlink.LinkByName(p.Device)
	if e != nil {
		return e
	}
	if e = netlink.LinkSetUp(link2); e != nil {
		return e
	}
	priority := 100
	if p.Device == "cldt-hov-b" {
		priority = 200
	}
	for _, a := range allowed {
		a := a
		if e = netlink.RouteAdd(&netlink.Route{LinkIndex: link2.Attrs().Index, Dst: &a, Priority: priority}); e != nil {
			return e
		}
	}
	// Equal routes use different devices; accept either ingress path. This is
	// confined to the experiment namespace, never the VM's host/CNI namespace.
	for _, name := range []string{"all", "default", p.Device} {
		if e = os.WriteFile("/proc/sys/net/ipv4/conf/"+name+"/rp_filter", []byte("0"), 0600); e != nil {
			return e
		}
	}
	actual, e := client.Device(p.Device)
	if e != nil {
		return e
	}
	if len(actual.Peers) != 1 || actual.PublicKey != key.PublicKey() || actual.ListenPort != p.Port {
		return fmt.Errorf("native device verification failed")
	}
	return nil
}

func socket(device, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var e error
		if err := c.Control(func(fd uintptr) {
			e = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, device)
		}); err != nil {
			return err
		}
		return e
	}}
	p, e := lc.ListenPacket(context.Background(), "udp", addr)
	if e != nil {
		return nil, e
	}
	return p.(*net.UDPConn), nil
}

func serve(device, addr, ready string, duration time.Duration) error {
	s, e := socket(device, addr)
	if e != nil {
		return e
	}
	defer s.Close()
	if e = os.WriteFile(ready, []byte("ready\n"), 0600); e != nil {
		return e
	}
	end := time.Now().Add(duration)
	buf := make([]byte, 2048)
	enc := json.NewEncoder(os.Stdout)
	for time.Now().Before(end) {
		if _, err := os.Stat(ready + ".stop"); err == nil {
			return nil
		}
		s.SetReadDeadline(time.Now().Add(time.Second))
		n, from, e := s.ReadFromUDP(buf)
		if e != nil {
			if n, ok := e.(net.Error); ok && n.Timeout() {
				continue
			}
			return e
		}
		_, e = s.WriteToUDP(buf[:n], from)
		if err := enc.Encode(map[string]any{"at": time.Now().UTC(), "source": from.String(), "payload": string(buf[:n]), "replyOK": e == nil}); err != nil {
			return err
		}
	}
	return nil
}

func probe(device, source, dest, ready string, count int, interval time.Duration, negative bool) error {
	s, e := socket(device, net.JoinHostPort(source, "0"))
	if e != nil {
		return e
	}
	defer s.Close()
	target, e := net.ResolveUDPAddr("udp", dest)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(os.Stdout)
	failed := 0
	for i := 0; i < count; i++ {
		start := time.Now()
		payload := fmt.Sprintf("%d-%d", os.Getpid(), i)
		s.SetDeadline(start.Add(time.Second))
		_, sendErr := s.WriteToUDP([]byte(payload), target)
		buf := make([]byte, 2048)
		n, from, recvErr := s.ReadFromUDP(buf)
		echo := sendErr == nil && recvErr == nil && from.String() == target.String() && string(buf[:n]) == payload
		ok := echo
		if negative {
			nerr, isTimeout := recvErr.(net.Error)
			ok = sendErr == nil && isTimeout && nerr.Timeout()
		}
		if !ok {
			failed++
		}
		record := map[string]any{"sequence": i, "startedAt": start.UTC(), "finishedAt": time.Now().UTC(), "source": s.LocalAddr().String(), "destination": dest, "expectedRejection": negative, "echo": echo, "ok": ok}
		if sendErr != nil {
			record["sendError"] = sendErr.Error()
		}
		if recvErr != nil {
			record["receiveError"] = recvErr.Error()
		}
		if e = enc.Encode(record); e != nil {
			return e
		}
		if ready != "" && i == 9 && failed == 0 {
			if e = os.WriteFile(ready, []byte("ready\n"), 0600); e != nil {
				return e
			}
		}
		if delay := interval - time.Since(start); delay > 0 {
			time.Sleep(delay)
		}
	}
	if failed != 0 {
		return fmt.Errorf("%d of %d requests failed", failed, count)
	}
	return nil
}

func run() error {
	mode := flag.String("mode", "", "keys, setup, serve or probe")
	device := flag.String("device", "", "experiment device")
	addr := flag.String("address", "", "listen address")
	dest := flag.String("destination", "", "probe destination")
	source := flag.String("source", "", "probe source IP")
	ready := flag.String("ready", "", "readiness receipt file")
	count := flag.Int("count", 100, "request count")
	interval := flag.Duration("interval", 20*time.Millisecond, "request interval")
	duration := flag.Duration("duration", 5*time.Minute, "server lifetime")
	negative := flag.Bool("negative", false, "require all requests rejected")
	flag.Parse()
	switch *mode {
	case "keys":
		keys := map[string]map[string]string{}
		for _, name := range []string{"rxA", "rxB", "old", "new"} {
			k, e := wgtypes.GeneratePrivateKey()
			if e != nil {
				return e
			}
			keys[name] = map[string]string{"private": k.String(), "public": k.PublicKey().String()}
		}
		return json.NewEncoder(os.Stdout).Encode(keys)
	case "setup":
		var p plan
		if e := json.NewDecoder(os.Stdin).Decode(&p); e != nil {
			return e
		}
		return setup(p)
	case "serve":
		return serve(*device, *addr, *ready, *duration)
	case "probe":
		return probe(*device, *source, *dest, *ready, *count, *interval, *negative)
	}
	return fmt.Errorf("unknown mode")
}

func main() {
	syscall.Umask(0077)
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
