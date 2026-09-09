package tunneldevice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/driver"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// An opt-in cross-VM experiment, not a production generation backend. A fresh
// protected plan and a start barrier are required on each of two Windows VMs.
func TestNativeStableOwnerPackets(t *testing.T) {
	path := os.Getenv("CLDT_NATIVE_STABLE_OWNER_PLAN")
	if path == "" {
		t.Skip("requires two Windows VMs, administrator and protected per-run plans")
	}
	var input struct {
		Names              [3]string
		Owner              tunnel.PeersFileDoc
		Generations        [2]tunnel.PeersFileDoc
		Remote, Run        string
		Spoof, RemoteSpoof string
	}
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	raw, e := os.ReadFile(path)
	must(e)
	must(json.Unmarshal(raw, &input))
	root := filepath.Dir(path)
	ownerPlan, e := Compile(input.Owner)
	must(e)
	remote, e := netip.ParseAddr(input.Remote)
	must(e)
	if len(input.Owner.Peers) != 0 || remote.BitLen() != ownerPlan.Address.Addr().BitLen() || input.Run == "" {
		t.Fatal("invalid isolated owner plan")
	}
	var spoof, remoteSpoof netip.Addr
	if input.Spoof != "" || input.RemoteSpoof != "" {
		spoof, e = netip.ParseAddr(input.Spoof)
		must(e)
		remoteSpoof, e = netip.ParseAddr(input.RemoteSpoof)
		must(e)
		seen := map[netip.Addr]bool{}
		for _, address := range []netip.Addr{ownerPlan.Address.Addr(), remote, spoof, remoteSpoof} {
			if address.BitLen() != remote.BitLen() || seen[address] || address.Is4In6() {
				t.Fatal("isolation addresses must be distinct and in one family")
			}
			seen[address] = true
		}
	}
	var plans [2]Plan
	for i, doc := range input.Generations {
		plans[i], e = Compile(doc)
		must(e)
		p := plans[i]
		if p.Address != ownerPlan.Address || len(p.Peers) != 1 || len(p.Routes) != 1 || p.Routes[0] != netip.PrefixFrom(remote, remote.BitLen()) || len(p.Peers[0].Allowed) != 1 || p.Peers[0].Allowed[0] != p.Routes[0] {
			t.Fatal("generation must allow exactly the remote test host")
		}
	}
	if plans[0].Key == plans[1].Key || plans[0].Peers[0].Key == plans[1].Peers[0].Key {
		t.Fatal("generation keys must be distinct")
	}

	// A retained cluster can route another worker's private IP through its
	// production tunnel. Reject that transport before creating any adapters.
	interfaces, e := winipcfg.GetIfTable2Ex(winipcfg.MibIfEntryNormal)
	must(e)
	for _, plan := range plans {
		var destination, source winipcfg.RawSockaddrInet
		must(destination.SetAddr(plan.Peers[0].Endpoint.Addr()))
		var route winipcfg.MibIPforwardRow2
		status, _, _ := windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetBestRoute2").Call(0, 0, 0, uintptr(unsafe.Pointer(&destination)), 0, uintptr(unsafe.Pointer(&route)), uintptr(unsafe.Pointer(&source)))
		if status != 0 {
			t.Fatalf("transport route lookup: %v", windows.Errno(status))
		}
		for _, row := range interfaces {
			if row.InterfaceLUID == route.InterfaceLUID && len(row.Alias()) >= 4 && row.Alias()[:4] == "cldt" {
				t.Fatal("experiment transport would use an existing cluster tunnel")
			}
		}
		t.Logf("transport endpoint %s uses LUID %d source %s", plan.Peers[0].Endpoint, route.InterfaceLUID, source.Addr())
	}
	addresses, e := winipcfg.GetUnicastIPAddressTable(windows.AF_UNSPEC)
	must(e)
	for _, r := range addresses {
		if r.Address.Addr() == ownerPlan.Address.Addr() || (spoof.IsValid() && r.Address.Addr() == spoof) {
			t.Fatal("test address already owned")
		}
	}
	routes, e := winipcfg.GetIPForwardTable2(windows.AF_UNSPEC)
	must(e)
	for _, r := range routes {
		if r.DestinationPrefix.Prefix() == plans[0].Routes[0] || (remoteSpoof.IsValid() && r.DestinationPrefix.Prefix() == netip.PrefixFrom(remoteSpoof, remoteSpoof.BitLen())) {
			t.Fatal("test host route already exists")
		}
	}
	owned := map[string]bool{}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			rows, e := winipcfg.GetIfTable2Ex(winipcfg.MibIfEntryNormal)
			if e != nil {
				t.Error(e)
				return
			}
			remaining := []string{}
			for _, r := range rows {
				if owned[r.Alias()] {
					remaining = append(remaining, r.Alias())
				}
			}
			if len(remaining) == 0 {
				t.Log("all owned stable-owner adapters removed")
				return
			}
			if time.Now().After(deadline) {
				t.Error("owned adapters remain", remaining)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	owner, e := New(input.Names[0])
	must(e)
	owned[input.Names[0]] = true
	t.Cleanup(func() {
		if e := owner.Close(); e != nil {
			t.Error(e)
		}
	})
	must(owner.Apply(input.Owner, 0))
	if spoof.IsValid() {
		must(owner.adapter.LUID().AddIPAddress(netip.PrefixFrom(spoof, spoof.BitLen())))
	}
	var adapters [2]*driver.Adapter
	for i := range adapters {
		n, e := New(input.Names[i+1])
		must(e)
		owned[input.Names[i+1]] = true
		adapters[i] = n.adapter
		index := i
		t.Cleanup(func() {
			if adapters[index] != nil {
				if e := adapters[index].Close(); e != nil {
					t.Error(e)
				}
			}
		})
		setupPlan := plans[i]
		if remoteSpoof.IsValid() {
			setupPlan.Peers = append([]Peer(nil), plans[i].Peers...)
			setupPlan.Peers[0].Allowed = append(append([]netip.Prefix(nil), plans[i].Peers[0].Allowed...), netip.PrefixFrom(remoteSpoof, remoteSpoof.BitLen()))
		}
		cfg, size, e := buildPeerUpdate(setupPlan, uint16(54871+i), nil)
		must(e)
		must(n.adapter.SetConfiguration(cfg, size))
		must(n.adapter.SetAdapterState(driver.AdapterStateUp))
		for _, family := range []winipcfg.AddressFamily{windows.AF_INET, windows.AF_INET6} {
			row, e := n.adapter.LUID().IPInterface(family)
			must(e)
			row.NLMTU = 1420
			row.UseAutomaticMetric = false
			row.Metric = 5
			row.WeakHostSend = true
			row.WeakHostReceive = true
			row.DadTransmits = 0
			must(row.Set())
		}
		must(n.adapter.LUID().AddRoute(plans[i].Routes[0], nextHop(plans[i].Routes[0]), uint32(10+i*90)))
		if remoteSpoof.IsValid() {
			prefix := netip.PrefixFrom(remoteSpoof, remoteSpoof.BitLen())
			must(n.adapter.LUID().AddRoute(prefix, nextHop(prefix), uint32(10+i*90)))
		}
	}
	events, e := os.OpenFile(filepath.Join(root, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(e)
	defer events.Close()
	var eventsMu sync.Mutex
	record := func(v any) { eventsMu.Lock(); defer eventsMu.Unlock(); must(json.NewEncoder(events).Encode(v)) }
	write := func(name string, v any) {
		b, e := json.Marshal(v)
		must(e)
		must(os.WriteFile(filepath.Join(root, name), b, 0600))
	}
	observe := func(stage string, selected int) {
		must(owner.Verify(input.Owner))
		rows, e := winipcfg.GetUnicastIPAddressTable(windows.AF_UNSPEC)
		must(e)
		for _, r := range rows {
			if r.Address.Addr() == ownerPlan.Address.Addr() && r.InterfaceLUID != owner.adapter.LUID() {
				t.Fatal("identity escaped stable owner")
			}
		}
		var source, dest, bestSource winipcfg.RawSockaddrInet
		kind := "route"
		address := ownerPlan.Address.Addr()
		if strings.HasPrefix(stage, "warmup-") {
			kind = "warmup-route"
			address = spoof
		}
		if strings.HasPrefix(stage, "source-") {
			kind = "source-route"
			address = spoof
		}
		must(source.SetAddr(address))
		must(dest.SetAddr(remote))
		var best winipcfg.MibIPforwardRow2
		// https://learn.microsoft.com/windows/win32/api/netioapi/nf-netioapi-getbestroute2
		status, _, _ := windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetBestRoute2").Call(0, 0, uintptr(unsafe.Pointer(&source)), uintptr(unsafe.Pointer(&dest)), 0, uintptr(unsafe.Pointer(&best)), uintptr(unsafe.Pointer(&bestSource)))
		if status != 0 {
			t.Fatalf("GetBestRoute2: %v", windows.Errno(status))
		}
		if best.InterfaceLUID != adapters[selected].LUID() {
			t.Fatalf("selected route LUID %d, want %d", best.InterfaceLUID, adapters[selected].LUID())
		}
		record(map[string]any{"kind": kind, "stage": stage, "at": time.Now().UTC(), "selected": selected, "luid": best.InterfaceLUID, "ownerLUID": owner.adapter.LUID(), "source": source.Addr().String(), "bestSource": bestSource.Addr().String()})
	}

	selectGeneration := func(selected int) {
		prefixes := []netip.Prefix{plans[0].Routes[0]}
		if remoteSpoof.IsValid() {
			prefixes = append(prefixes, netip.PrefixFrom(remoteSpoof, remoteSpoof.BitLen()))
		}
		for _, prefix := range prefixes {
			row, err := adapters[selected].LUID().Route(prefix, nextHop(prefix))
			must(err)
			row.Metric = 1
			must(row.Set())
			row, err = adapters[1-selected].LUID().Route(prefix, nextHop(prefix))
			must(err)
			row.Metric = 100
			must(row.Set())
		}
	}
	// Capture only public native counters, never Interface.PrivateKey or the
	// complete driver configuration. This also runs after a failed warmup.
	t.Cleanup(func() {
		var rows []map[string]any
		for i, adapter := range adapters {
			if adapter == nil {
				continue
			}
			cfg, err := adapter.Configuration()
			if err != nil {
				t.Error(err)
				continue
			}
			row := map[string]any{"generation": i, "luid": adapter.LUID(), "peerCount": cfg.PeerCount}
			if cfg.PeerCount == 1 {
				peer := cfg.FirstPeer()
				row["txBytes"] = peer.TxBytes
				row["rxBytes"] = peer.RxBytes
				row["lastHandshake"] = peer.LastHandshake
			}
			rows = append(rows, row)
		}
		body, err := json.Marshal(rows)
		if err == nil {
			err = os.WriteFile(filepath.Join(root, "counters.json"), body, 0600)
		}
		if err != nil {
			t.Error(err)
		}
	})
	observe("prepared", 0)
	network := "udp4"
	if remote.Is6() {
		network = "udp6"
	}
	local := ownerPlan.Address.Addr()
	server, e := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 54900)))
	must(e)
	var acceptSpoof atomic.Bool
	acceptSpoof.Store(true)
	var forbiddenReceived atomic.Int64
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, e := server.ReadFromUDPAddrPort(buf)
			if e != nil {
				done <- e
				return
			}
			if spoof.IsValid() && src.Addr() == remoteSpoof {
				kind := "source-control-received"
				if !acceptSpoof.Load() {
					kind = "forbidden-source"
					forbiddenReceived.Add(1)
				}
				eventsMu.Lock()
				err := json.NewEncoder(events).Encode(map[string]any{"kind": kind, "at": time.Now().UTC(), "source": src.String(), "bytes": n})
				eventsMu.Unlock()
				if err != nil {
					done <- err
					return
				}
			} else if src.Addr() != remote {
				done <- fmt.Errorf("unexpected source %s", src)
				return
			}
			if _, e = server.WriteToUDPAddrPort(buf[:n], src); e != nil {
				done <- e
				return
			}
		}
	}()
	defer func() {
		server.Close()
		e := <-done
		if e != nil && !bytes.Contains([]byte(e.Error()), []byte("closed network connection")) {
			t.Error(e)
		}
	}()
	client, e := net.DialUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)), net.UDPAddrFromAddrPort(netip.AddrPortFrom(remote, 54900)))
	must(e)
	defer client.Close()
	var spoofClient *net.UDPConn
	if spoof.IsValid() {
		spoofClient, e = net.DialUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(spoof, 0)), net.UDPAddrFromAddrPort(netip.AddrPortFrom(remote, 54900)))
		must(e)
		defer spoofClient.Close()
	}
	selectedGeneration := 0
	probeSpoof := func(phase string, sequence int, deny bool) error {
		begin := time.Now()
		must(spoofClient.SetDeadline(begin.Add(250 * time.Millisecond)))
		payload := []byte(fmt.Sprintf("%-64s", fmt.Sprintf("%s:source:%d", input.Run, sequence)))
		_, err := spoofClient.Write(payload)
		wrote := err == nil
		buf := make([]byte, 2048)
		n := 0
		if err == nil {
			n, err = spoofClient.Read(buf)
		}
		timedOut := wrote && os.IsTimeout(err)
		passed := wrote && ((deny && timedOut) || (!deny && err == nil && bytes.Equal(payload, buf[:n])))
		message := ""
		if err != nil {
			message = err.Error()
		}
		record(map[string]any{"kind": "source-probe", "generation": selectedGeneration, "phase": phase, "sequence": sequence, "at": begin.UTC(), "end": time.Now().UTC(), "source": spoofClient.LocalAddr().String(), "destination": spoofClient.RemoteAddr().String(), "bytes": len(payload), "deny": deny, "writeSucceeded": wrote, "readTimedOut": timedOut, "passed": passed, "error": message})
		if !passed {
			return fmt.Errorf("source probe failed: deny=%t write=%t timeout=%t", deny, wrote, timedOut)
		}
		return nil
	}
	write("ready.json", map[string]any{"run": input.Run, "pid": os.Getpid(), "names": input.Names, "at": time.Now().UTC()})
	var start time.Time
	deadline := time.Now().Add(180 * time.Second)
	for start.IsZero() {
		b, e := os.ReadFile(filepath.Join(root, "start.json"))
		if e == nil {
			var control struct {
				Run   string
				Start time.Time
			}
			must(json.Unmarshal(b, &control))
			if control.Run != input.Run || time.Until(control.Start) < 2*time.Second || time.Until(control.Start) > 120*time.Second {
				t.Fatal("invalid start barrier")
			}
			start = control.Start
			break
		}
		if !os.IsNotExist(e) {
			must(e)
		}
		if time.Now().After(deadline) {
			t.Fatal("start barrier timeout")
		}
		time.Sleep(200 * time.Millisecond)
	}
	exchange := func(sequence int, phase string) error {
		payload := []byte(fmt.Sprintf("%-64s", fmt.Sprintf("%s:%d", input.Run, sequence)))
		begin := time.Now()
		e := client.SetDeadline(begin.Add(time.Second))
		if e == nil {
			_, e = client.Write(payload)
		}
		buf := make([]byte, 2048)
		n := 0
		if e == nil {
			n, e = client.Read(buf)
		}
		if e == nil && !bytes.Equal(payload, buf[:n]) {
			e = fmt.Errorf("echo mismatch")
		}
		message := ""
		if e != nil {
			message = e.Error()
		}
		record(map[string]any{"kind": "packet", "phase": phase, "sequence": sequence, "at": begin.UTC(), "end": time.Now().UTC(), "source": client.LocalAddr().String(), "destination": client.RemoteAddr().String(), "bytes": len(payload), "error": message})
		return e
	}
	warmOK := false
	sourceControls := map[int]bool{}
	warmGeneration := -1
	controlSequence := 0
	for time.Until(start) > time.Second {
		if spoof.IsValid() {
			target := 0
			if left := time.Until(start); left <= 20*time.Second && left > 5*time.Second {
				target = 1
			}
			if target != warmGeneration {
				selectGeneration(target)
				label := "warmup-a"
				if target == 1 {
					label = "warmup-b"
				}
				observe(label, target)
				selectedGeneration = target
				warmGeneration = target
			}
		}
		if exchange(-1, "warmup") == nil {
			warmOK = true
		}
		if spoof.IsValid() {
			if probeSpoof("warmup-control", controlSequence, false) == nil {
				sourceControls[selectedGeneration] = true
			}
			controlSequence++
		}
		time.Sleep(100 * time.Millisecond)
	}
	if spoof.IsValid() && (!sourceControls[0] || !sourceControls[1] || selectedGeneration != 0) {
		t.Fatal("alternate source must pass controls on both generations and restore A")
	}
	if !warmOK {
		t.Fatal("no successful warmup exchange")
	}
	time.Sleep(time.Until(start))
	phases := []string{"baseline", "switch-b", "rollback-a", "reselect-b", "retire-a"}
	sequence, failures := 0, 0
	counts := map[string]int{}
	var currentPhase atomic.Value
	currentPhase.Store(phases[0])
	stop := make(chan struct{})
	sampled := make(chan struct{})
	var stopOnce sync.Once
	stopSampler := func() { stopOnce.Do(func() { close(stop) }); <-sampled }
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			label := currentPhase.Load().(string)
			if exchange(sequence, label) != nil {
				failures++
			}
			sequence++
			counts[label]++
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	defer stopSampler()
	if spoof.IsValid() {
		for i, adapter := range adapters {
			cfg, size, err := buildPeerUpdate(plans[i], uint16(54871+i), nil)
			must(err)
			must(adapter.SetConfiguration(cfg, size))
			actual, err := adapter.Configuration()
			must(err)
			if actual.PeerCount != 1 || actual.FirstPeer().PublicKey != plans[i].Peers[0].Key || actual.FirstPeer().AllowedIPsCount != 1 {
				t.Fatal("unexpected isolation peer configuration")
			}
			allowed := actual.FirstPeer().FirstAllowedIP()
			address := netip.AddrFrom16(allowed.Address)
			if allowed.AddressFamily == windows.AF_INET {
				var b [4]byte
				copy(b[:], allowed.Address[:4])
				address = netip.AddrFrom4(b)
			}
			if address != remote || int(allowed.Cidr) != remote.BitLen() {
				t.Fatal("native source allowance differs from authorized host")
			}
			record(map[string]any{"kind": "isolation-armed", "at": time.Now().UTC(), "generation": i, "allowedSource": remote.String(), "excludedSource": remoteSpoof.String()})
		}
		acceptSpoof.Store(false)
	}
	for phase, label := range phases {
		currentPhase.Store(label)
		record(map[string]any{"kind": "phase-start", "stage": label, "at": time.Now().UTC()})
		if phase > 0 {
			selected := 1
			if phase == 2 {
				selected = 0
			}
			if phase == 4 {
				must(adapters[0].Close())
				adapters[0] = nil
			} else {
				selectGeneration(selected)
			}
			selectedGeneration = selected
			observe(label, selected)
		}
		if spoof.IsValid() {
			// Allow both participants to finish native application before the
			// negative probes. Authorized sampling continues throughout.
			time.Sleep(time.Until(start.Add(time.Duration(phase)*10*time.Second + 2*time.Second)))
			observe("source-"+label, selectedGeneration)
			for attempt := 0; attempt < 2; attempt++ {
				must(probeSpoof(label, phase*2+attempt, true))
			}
		}
		until := start.Add(time.Duration(phase+1) * 10 * time.Second)
		time.Sleep(time.Until(until))
	}
	stopSampler()
	for _, label := range phases {
		if counts[label] == 0 {
			t.Fatal("phase had no samples", label)
		}
	}
	write("summary.json", map[string]any{"run": input.Run, "packets": sequence, "failures": failures, "phases": phases, "start": start, "end": time.Now().UTC()})
	// Keep the echo receiver alive beyond the peer sampler deadline and its
	// one-second request timeout. This is test teardown, not protocol drain.
	time.Sleep(3 * time.Second)
	if forbiddenReceived.Load() != 0 {
		t.Fatalf("%d forbidden source packets reached the application", forbiddenReceived.Load())
	}
	if failures != 0 {
		t.Fatalf("%d/%d qualified exchanges failed", failures, sequence)
	}
}
